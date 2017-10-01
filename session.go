package irmago

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/go-errors/errors"
	"github.com/mhe/gabi"
)

// PermissionHandler is a callback for providing permission for an IRMA session
// and specifying the attributes to be disclosed.
type PermissionHandler func(proceed bool, choice *DisclosureChoice)

// A Handler contains callbacks for communication to the user.
type Handler interface {
	StatusUpdate(action Action, status Status)
	Success(action Action)
	Cancelled(action Action)
	Failure(action Action, err *Error)
	UnsatisfiableRequest(action Action, missing AttributeDisjunctionList)

	AskIssuancePermission(request IssuanceRequest, ServerName string, callback PermissionHandler)
	AskVerificationPermission(request DisclosureRequest, ServerName string, callback PermissionHandler)
	AskSignaturePermission(request SignatureRequest, ServerName string, callback PermissionHandler)

	AskPin(remainingAttempts int, callback func(proceed bool, pin string))
}

// A session is an IRMA session.
type session struct {
	Action    Action
	Version   Version
	ServerURL string
	Handler   Handler

	info        *SessionInfo
	credManager *CredentialManager
	jwt         RequestorJwt
	irmaSession IrmaSession
	transport   *HTTPTransport
	choice      *DisclosureChoice
}

// Supported protocol versions. Minor version numbers should be reverse sorted.
var supportedVersions = map[int][]int{
	2: {2, 1},
}

func calcVersion(qr *Qr) (string, error) {
	// Parse range supported by server
	var minmajor, minminor, maxmajor, maxminor int
	var err error
	if minmajor, err = strconv.Atoi(string(qr.ProtocolVersion[0])); err != nil {
		return "", err
	}
	if minminor, err = strconv.Atoi(string(qr.ProtocolVersion[2])); err != nil {
		return "", err
	}
	if maxmajor, err = strconv.Atoi(string(qr.ProtocolMaxVersion[0])); err != nil {
		return "", err
	}
	if maxminor, err = strconv.Atoi(string(qr.ProtocolMaxVersion[2])); err != nil {
		return "", err
	}

	// Iterate supportedVersions in reverse sorted order (i.e. biggest major number first)
	keys := make([]int, 0, len(supportedVersions))
	for k := range supportedVersions {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))
	for _, major := range keys {
		for _, minor := range supportedVersions[major] {
			aboveMinimum := major > minmajor || (major == minmajor && minor >= minminor)
			underMaximum := major < maxmajor || (major == maxmajor && minor <= maxminor)
			if aboveMinimum && underMaximum {
				return fmt.Sprintf("%d.%d", major, minor), nil
			}
		}
	}
	return "", fmt.Errorf("No supported protocol version between %s and %s", qr.ProtocolVersion, qr.ProtocolMaxVersion)
}

// NewSession creates and starts a new IRMA session.
func NewSession(credManager *CredentialManager, qr *Qr, handler Handler) {
	session := &session{
		Action:      Action(qr.Type),
		ServerURL:   qr.URL,
		Handler:     handler,
		transport:   NewHTTPTransport(qr.URL),
		credManager: credManager,
	}
	version, err := calcVersion(qr)
	if err != nil {
		session.fail(&Error{ErrorCode: ErrorProtocolVersionNotSupported, Err: err})
		return
	}
	session.Version = Version(version)

	// Check if the action is one of the supported types
	switch session.Action {
	case ActionDisclosing: // nop
	case ActionSigning: // nop
	case ActionIssuing: // nop
	case ActionUnknown:
		fallthrough
	default:
		session.fail(&Error{ErrorCode: ErrorUnknownAction, Err: nil, Info: string(session.Action)})
		return
	}

	if !strings.HasSuffix(session.ServerURL, "/") {
		session.ServerURL += "/"
	}

	go session.start()

	return
}

func (session *session) fail(err *Error) {
	session.transport.Delete()
	err.Err = errors.Wrap(err.Err, 0)
	session.Handler.Failure(session.Action, err)
}

// start retrieves the first message in the IRMA protocol, checks if we can perform
// the request, and informs the user of the outcome.
func (session *session) start() {
	session.Handler.StatusUpdate(session.Action, StatusCommunicating)

	// Get the first IRMA protocol message and parse it
	session.info = &SessionInfo{}
	Err := session.transport.Get("jwt", session.info)
	if Err != nil {
		session.fail(Err.(*Error))
		return
	}

	var server string
	var err error
	session.jwt, server, err = parseRequestorJwt(session.Action, session.info.Jwt)
	if err != nil {
		session.fail(&Error{ErrorCode: ErrorInvalidJWT, Err: err})
		return
	}
	session.irmaSession = session.jwt.IrmaSession()
	session.irmaSession.SetContext(session.info.Context)
	session.irmaSession.SetNonce(session.info.Nonce)
	if session.Action == ActionIssuing {
		// Store which public keys the server will use
		for _, credreq := range session.irmaSession.(*IssuanceRequest).Credentials {
			credreq.KeyCounter = session.info.Keys[credreq.Credential.IssuerIdentifier()]
		}
	}

	missing := session.credManager.CheckSatisfiability(session.irmaSession.DisjunctionList())
	if len(missing) > 0 {
		session.Handler.UnsatisfiableRequest(session.Action, missing)
		// TODO: session.transport.Delete() on dialog cancel
		return
	}

	// Ask for permission to execute the session
	callback := PermissionHandler(func(proceed bool, choice *DisclosureChoice) {
		session.choice = choice
		session.irmaSession.SetDisclosureChoice(choice)
		go session.do(proceed)
	})
	session.Handler.StatusUpdate(session.Action, StatusConnected)
	switch session.Action {
	case ActionDisclosing:
		session.Handler.AskVerificationPermission(*session.irmaSession.(*DisclosureRequest), server, callback)
	case ActionSigning:
		session.Handler.AskSignaturePermission(*session.irmaSession.(*SignatureRequest), server, callback)
	case ActionIssuing:
		session.Handler.AskIssuancePermission(*session.irmaSession.(*IssuanceRequest), server, callback)
	default:
		panic("Invalid session type") // does not happen, session.Action has been checked earlier
	}
}

func (session *session) do(proceed bool) {
	if !proceed {
		session.transport.Delete()
		session.Handler.Cancelled(session.Action)
		return
	}
	session.Handler.StatusUpdate(session.Action, StatusCommunicating)

	if !session.irmaSession.Distributed(session.credManager.Store) {
		var message interface{}
		var err error
		switch session.Action {
		case ActionSigning:
			message, err = session.credManager.Proofs(session.choice, session.irmaSession, true)
		case ActionDisclosing:
			message, err = session.credManager.Proofs(session.choice, session.irmaSession, false)
		case ActionIssuing:
			message, err = session.credManager.IssueCommitments(session.irmaSession.(*IssuanceRequest))
		}
		if err != nil {
			session.fail(&Error{ErrorCode: ErrorCrypto, Err: err})
			return
		}
		session.sendResponse(message)
	} else {
		var builders gabi.ProofBuilderList
		var err error
		switch session.Action {
		case ActionSigning:
			fallthrough
		case ActionDisclosing:
			builders, err = session.credManager.ProofBuilders(session.choice)
		case ActionIssuing:
			builders, err = session.credManager.IssuanceProofBuilders(session.irmaSession.(*IssuanceRequest))
		}
		if err != nil {
			session.fail(&Error{ErrorCode: ErrorCrypto, Err: err})
		}

		startKeyshareSession(session.credManager, session.irmaSession, builders, session, session.Handler)
	}
}

func (session *session) KeyshareDone(message interface{}) {
	session.sendResponse(message)
}

func (session *session) KeyshareCancelled() {
	session.transport.Delete()
	session.Handler.Cancelled(session.Action)
}

func (session *session) KeyshareBlocked(duration int) {
	session.fail(&Error{ErrorCode: ErrorKeyshareBlocked, Info: strconv.Itoa(duration)})
}

func (session *session) KeyshareError(err error) {
	session.fail(&Error{ErrorCode: ErrorKeyshare, Err: err})
}

type disclosureResponse string

func (session *session) sendResponse(message interface{}) {
	var log *LogEntry
	var err error

	switch session.Action {
	case ActionSigning:
		fallthrough
	case ActionDisclosing:
		var response disclosureResponse
		if err = session.transport.Post("proofs", &response, message); err != nil {
			session.fail(err.(*Error))
			return
		}
		if response != "VALID" {
			session.fail(&Error{ErrorCode: ErrorRejected, Info: string(response)})
			return
		}
		log, err = session.createLogEntry(message.(gabi.ProofList)) // TODO err
	case ActionIssuing:
		response := []*gabi.IssueSignatureMessage{}
		if err = session.transport.Post("commitments", &response, message); err != nil {
			session.fail(err.(*Error))
			return
		}
		if err = session.credManager.ConstructCredentials(response, session.irmaSession.(*IssuanceRequest)); err != nil {
			session.fail(&Error{Err: err, ErrorCode: ErrorCrypto})
			return
		}
		log, err = session.createLogEntry(message.(*gabi.IssueCommitmentMessage).Proofs) // TODO err
	}

	session.credManager.addLogEntry(log) // TODO err
	session.Handler.Success(session.Action)
}
