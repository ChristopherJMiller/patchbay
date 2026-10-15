package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// Body limits for control API answers: the observations document, and
// anything else (an error, a firmware upload's answer).
const (
	maxObservationsBody = 16 << 20
	maxAnswerBody       = 64 << 10
)

// agentRequest sends one request to the control API with the token. A
// body is sent with size as its Content-Length (an empty one as no body,
// not as chunked).
func agentRequest(ctx context.Context, conn *AgentConn, method, path string, body io.Reader, size int64) (*http.Response, error) {
	if body != nil && size == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, conn.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)
	return conn.HTTP.Do(req)
}

// answerError is the error message of a control API answer: the "error"
// field both api.Error and api.FirmwareUploadResponse carry, else the
// status text.
func answerError(res *http.Response) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxAnswerBody)).Decode(&body); err != nil || body.Error == "" {
		return http.StatusText(res.StatusCode)
	}
	return body.Error
}

// observations asks the agent for its document through a fresh
// connection.
func observations(ctx context.Context, opener AgentOpener) (agentapi.Observations, error) {
	var doc agentapi.Observations
	if opener == nil {
		return doc, errors.New("no way to reach the agent")
	}
	conn, err := opener.OpenAgent(ctx)
	if err != nil {
		return doc, err
	}
	defer func() { _ = conn.Close() }()
	res, err := agentRequest(ctx, conn, http.MethodGet, agentapi.PathObservations, nil, 0)
	if err != nil {
		return doc, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return doc, fmt.Errorf("GET %s: %d %s", agentapi.PathObservations, res.StatusCode, answerError(res))
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxObservationsBody)).Decode(&doc); err != nil {
		return doc, fmt.Errorf("GET %s: %w", agentapi.PathObservations, err)
	}
	return doc, nil
}
