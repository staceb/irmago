package irmaclient

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwesterb/go-atum"
	"github.com/dgrijalva/jwt-go"
	"github.com/go-errors/errors"
	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/gabi/big"
	"github.com/privacybydesign/irmago"
)

// This file contains an implementation of the client side of the keyshare protocol,
// as well as the keyshareSessionHandler which is used to communicate with the user
// (currently only Client).

// KeysharePinRequestor is used to asking the user for his PIN.
type KeysharePinRequestor interface {
	RequestPin(remainingAttempts int, callback PinHandler)
}

type keyshareSessionHandler interface {
	KeyshareDone(message interface{})
	KeyshareCancelled()
	KeyshareBlocked(manager irma.SchemeManagerIdentifier, duration int)
	KeyshareEnrollmentIncomplete(manager irma.SchemeManagerIdentifier)
	KeyshareEnrollmentDeleted(manager irma.SchemeManagerIdentifier)
	// In errors the manager may be nil, as not all keyshare errors have a clearly associated scheme manager
	KeyshareError(manager *irma.SchemeManagerIdentifier, err error)
	KeysharePin()
	KeysharePinOK()
}

type keyshareSession struct {
	sessionHandler   keyshareSessionHandler
	pinRequestor     KeysharePinRequestor
	builders         gabi.ProofBuilderList
	session          irma.SessionRequest
	conf             *irma.Configuration
	keyshareServers  map[irma.SchemeManagerIdentifier]*keyshareServer
	keyshareServer   *keyshareServer // The one keyshare server in use in case of issuance
	transports       map[irma.SchemeManagerIdentifier]*irma.HTTPTransport
	issuerProofNonce *big.Int
	timestamp        *atum.Timestamp
	pinCheck         bool
}

type keyshareServer struct {
	Username                string `json:"username"`
	Nonce                   []byte `json:"nonce"`
	SchemeManagerIdentifier irma.SchemeManagerIdentifier
	token                   string
}

type keyshareEnrollment struct {
	Username string  `json:"username"`
	Pin      string  `json:"pin"`
	Email    *string `json:"email"`
	Language string  `json:"language"`
}

type keyshareChangepin struct {
	Username string `json:"id"`
	OldPin   string `json:"oldpin"`
	NewPin   string `json:"newpin"`
}

type keyshareAuthorization struct {
	Status     string   `json:"status"`
	Candidates []string `json:"candidates"`
}

type keysharePinMessage struct {
	Username string `json:"id"`
	Pin      string `json:"pin"`
}

type keysharePinStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type publicKeyIdentifier struct {
	Issuer  string `json:"issuer"`
	Counter uint   `json:"counter"`
}

func (pki *publicKeyIdentifier) UnmarshalText(text []byte) error {
	str := string(text)
	index := strings.LastIndex(str, "-")
	if index == -1 {
		return errors.New("Invalid publicKeyIdentifier")
	}
	counter, err := strconv.Atoi(str[index+1:])
	if err != nil {
		return err
	}
	*pki = publicKeyIdentifier{Issuer: str[:index], Counter: uint(counter)}
	return nil
}

func (pki *publicKeyIdentifier) MarshalText() (text []byte, err error) {
	return []byte(fmt.Sprintf("%s-%d", pki.Issuer, pki.Counter)), nil
}

type proofPCommitmentMap struct {
	Commitments map[publicKeyIdentifier]*gabi.ProofPCommitment `json:"c"`
}

const (
	kssUsernameHeader = "X-IRMA-Keyshare-Username"
	kssVersionHeader  = "X-IRMA-Keyshare-ProtocolVersion"
	kssAuthHeader     = "Authorization"
	kssAuthorized     = "authorized"
	kssTokenExpired   = "expired"
	kssPinSuccess     = "success"
	kssPinFailure     = "failure"
	kssPinError       = "error"
)

func newKeyshareServer(schemeManagerIdentifier irma.SchemeManagerIdentifier) (ks *keyshareServer, err error) {
	ks = &keyshareServer{
		Nonce: make([]byte, 32),
		SchemeManagerIdentifier: schemeManagerIdentifier,
	}
	_, err = rand.Read(ks.Nonce)
	return
}

func (ks *keyshareServer) HashedPin(pin string) string {
	hash := sha256.Sum256(append(ks.Nonce, []byte(pin)...))
	// We must be compatible with the old Android app here,
	// which uses Base64.encodeToString(hash, Base64.DEFAULT),
	// which appends a newline.
	return base64.StdEncoding.EncodeToString(hash[:]) + "\n"
}

// startKeyshareSession starts and completes the entire keyshare protocol with all involved keyshare servers
// for the specified session, merging the keyshare proofs into the specified ProofBuilder's.
// The user's pin is retrieved using the KeysharePinRequestor, repeatedly, until either it is correct; or the
// user cancels; or one of the keyshare servers blocks us.
// Error, blocked or success of the keyshare session is reported back to the keyshareSessionHandler.
func startKeyshareSession(
	sessionHandler keyshareSessionHandler,
	pin KeysharePinRequestor,
	builders gabi.ProofBuilderList,
	session irma.SessionRequest,
	conf *irma.Configuration,
	keyshareServers map[irma.SchemeManagerIdentifier]*keyshareServer,
	issuerProofNonce *big.Int,
	timestamp *atum.Timestamp,
) {
	ksscount := 0
	for managerID := range session.Identifiers().SchemeManagers {
		if conf.SchemeManagers[managerID].Distributed() {
			ksscount++
			if _, enrolled := keyshareServers[managerID]; !enrolled {
				err := errors.New("Not enrolled to keyshare server of scheme manager " + managerID.String())
				sessionHandler.KeyshareError(&managerID, err)
				return
			}
		}
	}
	if _, issuing := session.(*irma.IssuanceRequest); issuing && ksscount > 1 {
		err := errors.New("Issuance session involving more than one keyshare servers are not supported")
		sessionHandler.KeyshareError(nil, err)
		return
	}

	ks := &keyshareSession{
		session:          session,
		builders:         builders,
		sessionHandler:   sessionHandler,
		transports:       map[irma.SchemeManagerIdentifier]*irma.HTTPTransport{},
		pinRequestor:     pin,
		conf:             conf,
		keyshareServers:  keyshareServers,
		issuerProofNonce: issuerProofNonce,
		timestamp:        timestamp,
		pinCheck:         false,
	}

	for managerID := range session.Identifiers().SchemeManagers {
		scheme := ks.conf.SchemeManagers[managerID]
		if !scheme.Distributed() {
			continue
		}

		ks.keyshareServer = ks.keyshareServers[managerID]
		transport := irma.NewHTTPTransport(scheme.KeyshareServer)
		transport.SetHeader(kssUsernameHeader, ks.keyshareServer.Username)
		transport.SetHeader(kssAuthHeader, "Bearer "+ks.keyshareServer.token)
		transport.SetHeader(kssVersionHeader, "2")
		ks.transports[managerID] = transport

		// Try to parse token as a jwt to see if it is still valid; if so we don't need to ask for the PIN
		parser := new(jwt.Parser)
		parser.SkipClaimsValidation = true // We want to verify expiry on our own below so we can add leeway
		claims := jwt.StandardClaims{}
		_, err := parser.ParseWithClaims(ks.keyshareServer.token, &claims, ks.conf.KeyshareServerKeyFunc(managerID))
		if err != nil {
			irma.Logger.Info("Keyshare server token invalid, asking for PIN")
			irma.Logger.Debug("Token: ", ks.keyshareServer.token)
			ks.pinCheck = true
			continue
		}
		// Add a minute of leeway for possible clockdrift with the server,
		// and for the rest of the protocol to take place with this token
		if !claims.VerifyExpiresAt(time.Now().Add(1*time.Minute).Unix(), true) {
			irma.Logger.Info("Keyshare server token expires too soon, asking for PIN")
			irma.Logger.Debug("Token: ", ks.keyshareServer.token)
			ks.pinCheck = true
		}
	}

	if ks.pinCheck {
		ks.sessionHandler.KeysharePin()
		ks.VerifyPin(-1)
	} else {
		ks.GetCommitments()
	}
}

func (ks *keyshareSession) fail(manager irma.SchemeManagerIdentifier, err error) {
	serr, ok := err.(*irma.SessionError)
	if ok {
		if serr.RemoteError != nil && len(serr.RemoteError.ErrorName) > 0 {
			switch serr.RemoteError.ErrorName {
			case "USER_NOT_FOUND":
				ks.sessionHandler.KeyshareEnrollmentDeleted(manager)
			case "USER_NOT_REGISTERED":
				ks.sessionHandler.KeyshareEnrollmentIncomplete(manager)
			case "USER_BLOCKED":
				duration, err := strconv.Atoi(serr.RemoteError.Message)
				if err != nil { // Not really clear what to do with duration, but should never happen anyway
					duration = -1
				}
				ks.sessionHandler.KeyshareBlocked(manager, duration)
			default:
				ks.sessionHandler.KeyshareError(&manager, err)
			}
		}
	} else {
		ks.sessionHandler.KeyshareError(&manager, err)
	}
}

// Ask for a pin, repeatedly if necessary, and either continue the keyshare protocol
// with authorization, or stop the keyshare protocol and inform of failure.
func (ks *keyshareSession) VerifyPin(attempts int) {
	ks.pinRequestor.RequestPin(attempts, PinHandler(func(proceed bool, pin string) {
		if !proceed {
			ks.sessionHandler.KeyshareCancelled()
			return
		}
		success, attemptsRemaining, blocked, manager, err := ks.verifyPinAttempt(pin)
		if err != nil {
			ks.sessionHandler.KeyshareError(&manager, err)
			return
		}
		if blocked != 0 {
			ks.sessionHandler.KeyshareBlocked(manager, blocked)
			return
		}
		if success {
			ks.sessionHandler.KeysharePinOK()
			ks.GetCommitments()
			return
		}
		// Not successful but no error and not yet blocked: try again
		ks.VerifyPin(attemptsRemaining)
	}))
}

func verifyPinWorker(pin string, kss *keyshareServer, transport *irma.HTTPTransport) (
	success bool, tries int, blocked int, err error) {
	pinmsg := keysharePinMessage{Username: kss.Username, Pin: kss.HashedPin(pin)}
	pinresult := &keysharePinStatus{}
	err = transport.Post("users/verify/pin", pinresult, pinmsg)
	if err != nil {
		return
	}

	switch pinresult.Status {
	case kssPinSuccess:
		success = true
		kss.token = pinresult.Message
		transport.SetHeader(kssAuthHeader, kss.token)
		return
	case kssPinFailure:
		tries, err = strconv.Atoi(pinresult.Message)
		return
	case kssPinError:
		blocked, err = strconv.Atoi(pinresult.Message)
		return
	default:
		err = &irma.SessionError{
			Err:       errors.New("Keyshare server returned unrecognized PIN status"),
			ErrorType: irma.ErrorServerResponse,
			Info:      "Keyshare server returned unrecognized PIN status",
		}
		return
	}
}

// Verify the specified pin at each of the keyshare servers involved in the specified session.
// - If the pin did not verify at one of the keyshare servers but there are attempts remaining,
// the amount of remaining attempts is returned as the second return value.
// - If the pin did not verify at one of the keyshare servers and there are no attempts remaining,
// the amount of time for which we are blocked at the keyshare server is returned as the third
// parameter.
// - If this or anything else (specified in err) goes wrong, success will be false.
// If all is ok, success will be true.
func (ks *keyshareSession) verifyPinAttempt(pin string) (
	success bool, tries int, blocked int, manager irma.SchemeManagerIdentifier, err error) {
	for manager = range ks.session.Identifiers().SchemeManagers {
		if !ks.conf.SchemeManagers[manager].Distributed() {
			continue
		}

		kss := ks.keyshareServers[manager]
		transport := ks.transports[manager]
		success, tries, blocked, err = verifyPinWorker(pin, kss, transport)
		if !success {
			return
		}
	}
	return
}

// GetCommitments gets the commitments (first message in Schnorr zero-knowledge protocol)
// of all keyshare servers of their part of the private key, and merges these commitments
// in our own proof builders.
func (ks *keyshareSession) GetCommitments() {
	pkids := map[irma.SchemeManagerIdentifier][]*publicKeyIdentifier{}
	commitments := map[publicKeyIdentifier]*gabi.ProofPCommitment{}

	// For each scheme manager, build a list of public keys under this manager
	// that we will use in the keyshare protocol with the keyshare server of this manager
	for _, builder := range ks.builders {
		pk := builder.PublicKey()
		managerID := irma.NewIssuerIdentifier(pk.Issuer).SchemeManagerIdentifier()
		if !ks.conf.SchemeManagers[managerID].Distributed() {
			continue
		}
		if _, contains := pkids[managerID]; !contains {
			pkids[managerID] = []*publicKeyIdentifier{}
		}
		pkids[managerID] = append(pkids[managerID], &publicKeyIdentifier{Issuer: pk.Issuer, Counter: pk.Counter})
	}

	// Now inform each keyshare server of with respect to which public keys
	// we want them to send us commitments
	for managerID := range ks.session.Identifiers().SchemeManagers {
		if !ks.conf.SchemeManagers[managerID].Distributed() {
			continue
		}

		transport := ks.transports[managerID]
		comms := &proofPCommitmentMap{}
		err := transport.Post("prove/getCommitments", comms, pkids[managerID])
		if err != nil {
			if err.(*irma.SessionError).RemoteError != nil &&
				err.(*irma.SessionError).RemoteError.Status == http.StatusForbidden && !ks.pinCheck {
				// JWT may be out of date due to clock drift; request pin and try again
				// (but only if we did not ask for a PIN earlier)
				ks.pinCheck = false
				ks.sessionHandler.KeysharePin()
				ks.VerifyPin(-1)
				return
			}
			ks.sessionHandler.KeyshareError(&managerID, err)
			return
		}
		for pki, c := range comms.Commitments {
			commitments[pki] = c
		}
	}

	// Merge in the commitments
	for _, builder := range ks.builders {
		pk := builder.PublicKey()
		pki := publicKeyIdentifier{Issuer: pk.Issuer, Counter: pk.Counter}
		comm, distributed := commitments[pki]
		if !distributed {
			continue
		}
		builder.MergeProofPCommitment(comm)
	}

	ks.GetProofPs()
}

// GetProofPs uses the combined commitments of all keyshare servers and ourself
// to calculate the challenge, which is sent to the keyshare servers in order to
// receive their responses (2nd and 3rd message in Schnorr zero-knowledge protocol).
func (ks *keyshareSession) GetProofPs() {
	_, issig := ks.session.(*irma.SignatureRequest)
	challenge := ks.builders.Challenge(ks.session.Base().GetContext(), ks.session.GetNonce(ks.timestamp), issig)

	// Post the challenge, obtaining JWT's containing the ProofP's
	responses := map[irma.SchemeManagerIdentifier]string{}
	for managerID := range ks.session.Identifiers().SchemeManagers {
		transport, distributed := ks.transports[managerID]
		if !distributed {
			continue
		}
		var j string
		err := transport.Post("prove/getResponse", &j, challenge)
		if err != nil {
			ks.sessionHandler.KeyshareError(&managerID, err)
			return
		}
		responses[managerID] = j
	}

	ks.Finish(challenge, responses)
}

// Finish the keyshare protocol: in case of issuance, put the keyshare jwt in the
// IssueCommitmentMessage; in case of disclosure and signing, parse each keyshare jwt,
// merge in the received ProofP's, and finish.
func (ks *keyshareSession) Finish(challenge *big.Int, responses map[irma.SchemeManagerIdentifier]string) {
	switch ks.session.(type) {
	case *irma.DisclosureRequest: // Can't use fallthrough in a type switch in go
		ks.finishDisclosureOrSigning(challenge, responses)
	case *irma.SignatureRequest: // So we have to do this in a separate method
		ks.finishDisclosureOrSigning(challenge, responses)
	case *irma.IssuanceRequest:
		// Calculate IssueCommitmentMessage, without merging in any of the received ProofP's:
		// instead, include the keyshare server's JWT in the IssueCommitmentMessage for the
		// issuance server to verify
		list, err := ks.builders.BuildDistributedProofList(challenge, nil)
		if err != nil {
			ks.sessionHandler.KeyshareError(&ks.keyshareServer.SchemeManagerIdentifier, err)
			return
		}
		message := &gabi.IssueCommitmentMessage{Proofs: list, Nonce2: ks.issuerProofNonce}
		message.ProofPjwts = map[string]string{}
		for manager, response := range responses {
			message.ProofPjwts[manager.String()] = response
		}
		ks.sessionHandler.KeyshareDone(message)
	}
}

func (ks *keyshareSession) finishDisclosureOrSigning(challenge *big.Int, responses map[irma.SchemeManagerIdentifier]string) {
	proofPs := make([]*gabi.ProofP, len(ks.builders))
	for i, builder := range ks.builders {
		// Parse each received JWT
		managerID := irma.NewIssuerIdentifier(builder.PublicKey().Issuer).SchemeManagerIdentifier()
		if !ks.conf.SchemeManagers[managerID].Distributed() {
			continue
		}
		claims := struct {
			jwt.StandardClaims
			ProofP *gabi.ProofP
		}{}
		parser := new(jwt.Parser)
		parser.SkipClaimsValidation = true // no need to abort due to clock drift issues
		if _, err := parser.ParseWithClaims(responses[managerID], &claims, ks.conf.KeyshareServerKeyFunc(managerID)); err != nil {
			ks.sessionHandler.KeyshareError(&managerID, err)
			return
		}
		proofPs[i] = claims.ProofP
	}

	// Create merged proofs and finish protocol
	list, err := ks.builders.BuildDistributedProofList(challenge, proofPs)
	if err != nil {
		ks.sessionHandler.KeyshareError(nil, err)
		return
	}
	ks.sessionHandler.KeyshareDone(list)
}
