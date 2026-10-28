package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DecisionResponse is the typed answer auto mode consumes for one command
// review. It is deliberately small: a decision, how sure the model is, the
// impact if the command is unwanted, and whether the user authorized it.
type DecisionResponse struct {
	Decision   string  `json:"decision"`
	Confidence float64 `json:"confidence"`
	Risk       string  `json:"risk"`
	Authorized bool    `json:"authorized"`
}

// maxDecisionErrorBody bounds how much of a failed audit response is retained
// for diagnosis. The body may name the serving model or provider, so it never
// travels back to the caller.
const maxDecisionErrorBody = 2 << 10

// Decide asks the gateway's command-audit endpoint for a typed decision about
// one review state, attributed to the public model the turn is running as.
//
// This is a separate call shape from Complete: it posts the isolated review
// state and reads a decision object, not a chat completion. It uses the same
// authenticated endpoint selection as every other call, so a signed-in session
// talks to the app API and an API-key deployment talks to the public API.
func (p *HTTPProvider) Decide(ctx context.Context, model string, state json.RawMessage) (DecisionResponse, error) {
	body, err := json.Marshal(struct {
		Model string          `json:"model"`
		State json.RawMessage `json:"state"`
	}{Model: model, State: state})
	if err != nil {
		return DecisionResponse{}, fmt.Errorf("llm: encoding the audit state: %w", err)
	}

	request, err := p.NewEndpointRequest(ctx, http.MethodPost, "/audit/command", bytes.NewReader(body))
	if err != nil {
		return DecisionResponse{}, err
	}

	response, err := p.client().Do(request)
	if err != nil {
		return DecisionResponse{}, fmt.Errorf("llm: audit transport: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		// Read and discard a bounded body: keep the failure diagnosable without
		// letting an upstream message reach the caller.
		_, _ = io.ReadAll(io.LimitReader(response.Body, maxDecisionErrorBody))

		return DecisionResponse{}, fmt.Errorf("llm: audit check returned %d", response.StatusCode)
	}

	var decision DecisionResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&decision); err != nil {
		return DecisionResponse{}, fmt.Errorf("llm: reading the audit decision: %w", err)
	}

	return decision, nil
}
