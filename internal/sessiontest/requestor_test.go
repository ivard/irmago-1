package sessiontest

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"testing"

	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/internal/test"
	"github.com/privacybydesign/irmago/server"
	"github.com/stretchr/testify/require"
)

type testOption int

const (
	retryPost testOption = 1 << iota
)

func requestorSessionHelper(t *testing.T, request irma.SessionRequest, options ...testOption) *server.SessionResult {
	StartIrmaServer(t)
	defer StopIrmaServer()

	client, _ := parseStorage(t)
	defer test.ClearTestStorage(t)

	clientChan := make(chan *SessionResult)
	serverChan := make(chan *server.SessionResult)

	qr, token, err := irmaServer.StartSession(request, func(result *server.SessionResult) {
		serverChan <- result
	})
	require.NoError(t, err)

	h := &TestHandler{t: t, c: clientChan, client: client}
	j, err := json.Marshal(qr)
	require.NoError(t, err)
	client.NewSession(string(j), h)
	clientResult := <-clientChan
	if clientResult != nil {
		require.NoError(t, clientResult.Err)
	}

	serverResult := <-serverChan
	require.Equal(t, token, serverResult.Token)

	opts := 0
	for _, o := range options {
		opts |= int(o)
	}
	if opts&int(retryPost) > 0 {
		req, err := http.NewRequest(http.MethodPost,
			qr.URL+"/proofs",
			bytes.NewBuffer([]byte(h.result)),
		)
		require.NoError(t, err)
		req.Header.Add("Content-Type", "application/json")
		res, err := new(http.Client).Do(req)
		require.NoError(t, err)
		require.True(t, res.StatusCode < 300)
		_, err = ioutil.ReadAll(res.Body)
		require.NoError(t, err)
	}

	return serverResult
}

func TestRequestorSignatureSession(t *testing.T) {
	id := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")

	for _, opt := range []testOption{0, retryPost} {
		serverResult := requestorSessionHelper(t, &irma.SignatureRequest{
			Message: "message",
			DisclosureRequest: irma.DisclosureRequest{
				BaseRequest: irma.BaseRequest{Type: irma.ActionSigning},
				Content: irma.AttributeDisjunctionList([]*irma.AttributeDisjunction{{
					Label:      "foo",
					Attributes: []irma.AttributeTypeIdentifier{id},
				}}),
			},
		}, opt)

		require.Nil(t, serverResult.Err)
		require.Equal(t, irma.ProofStatusValid, serverResult.ProofStatus)
		require.NotEmpty(t, serverResult.Disclosed)
		require.Equal(t, id, serverResult.Disclosed[0].Identifier)
		require.Equal(t, "456", serverResult.Disclosed[0].Value["en"])
	}
}

func TestRequestorDisclosureSession(t *testing.T) {
	for _, opt := range []testOption{0, retryPost} {
		id := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")
		request := &irma.DisclosureRequest{
			BaseRequest: irma.BaseRequest{Type: irma.ActionDisclosing},
			Content: irma.AttributeDisjunctionList([]*irma.AttributeDisjunction{{
				Label:      "foo",
				Attributes: []irma.AttributeTypeIdentifier{id},
			}}),
		}
		serverResult := testRequestorDisclosure(t, request, opt)
		require.Len(t, serverResult.Disclosed, 1)
		require.Equal(t, id, serverResult.Disclosed[0].Identifier)
		require.Equal(t, "456", serverResult.Disclosed[0].Value["en"])
	}
}

func TestRequestorDisclosureMultipleAttrs(t *testing.T) {
	request := &irma.DisclosureRequest{
		BaseRequest: irma.BaseRequest{Type: irma.ActionDisclosing},
		Content: irma.AttributeDisjunctionList([]*irma.AttributeDisjunction{{
			Label:      "foo",
			Attributes: []irma.AttributeTypeIdentifier{irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")},
		}, {
			Label:      "bar",
			Attributes: []irma.AttributeTypeIdentifier{irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.level")},
		}}),
	}
	serverResult := testRequestorDisclosure(t, request)
	require.Len(t, serverResult.Disclosed, 2)
}

func testRequestorDisclosure(t *testing.T, request *irma.DisclosureRequest, options ...testOption) *server.SessionResult {
	serverResult := requestorSessionHelper(t, request, options...)
	require.Nil(t, serverResult.Err)
	require.Equal(t, irma.ProofStatusValid, serverResult.ProofStatus)
	return serverResult
}

func TestRequestorIssuanceSession(t *testing.T) {
	testRequestorIssuance(t, false)
}

func TestRequestorCombinedSessionMultipleAttributes(t *testing.T) {
	var ir irma.IssuanceRequest
	require.NoError(t, irma.UnmarshalValidate([]byte(`{
		"type":"issuing",
		"credentials": [
			{
				"credential":"irma-demo.MijnOverheid.root",
				"attributes" : {
					"BSN":"12345"
				}
			}
		],
		"disclose" : [
			{
				"label":"Initialen",
				"attributes":["irma-demo.RU.studentCard.studentCardNumber"]
			},
			{
				"label":"Achternaam",
				"attributes" : ["irma-demo.RU.studentCard.studentID"]
			},
			{
				"label":"Geboortedatum",
				"attributes":["irma-demo.RU.studentCard.university"]
			}
		]
	}`), &ir))

	require.Equal(t, server.StatusDone, requestorSessionHelper(t, &ir).Status)
}

func testRequestorIssuance(t *testing.T, keyshare bool) {
	attrid := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")
	request := &irma.IssuanceRequest{
		BaseRequest: irma.BaseRequest{Type: irma.ActionIssuing},
	}
	request.Credentials = []*irma.CredentialRequest{{
		CredentialTypeID: irma.NewCredentialTypeIdentifier("irma-demo.RU.studentCard"),
		Attributes: map[string]string{
			"university":        "Radboud",
			"studentCardNumber": "31415927",
			"studentID":         "s1234567",
			"level":             "42",
		},
	}, {
		CredentialTypeID: irma.NewCredentialTypeIdentifier("irma-demo.MijnOverheid.root"),
		Attributes: map[string]string{
			"BSN": "299792458",
		},
	}}
	if keyshare {
		request.Credentials = append(request.Credentials, &irma.CredentialRequest{
			CredentialTypeID: irma.NewCredentialTypeIdentifier("test.test.mijnirma"),
			Attributes:       map[string]string{"email": "testusername"},
		})
	}
	request.Disclose = []*irma.AttributeDisjunction{{
		Label:      "foo",
		Attributes: []irma.AttributeTypeIdentifier{attrid},
	}}

	result := requestorSessionHelper(t, request)
	require.Nil(t, result.Err)
	require.Equal(t, irma.ProofStatusValid, result.ProofStatus)
	require.NotEmpty(t, result.Disclosed)
	require.Equal(t, attrid, result.Disclosed[0].Identifier)
	require.Equal(t, "456", result.Disclosed[0].Value["en"])
}
