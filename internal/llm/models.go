package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ModelCard is one public model exposed by the Gateway. It deliberately holds
// only public product metadata; the upstream provider and model stay hidden.
type ModelCard struct {
	ID              string `json:"id"`
	Description     string `json:"description"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

// ModelCatalog is implemented by a provider that can list models available to
// the signed-in account.
type ModelCatalog interface {
	Models(context.Context) ([]ModelCard, error)
}

// Models returns the Gateway's enabled public models, using the same OAuth-
// first authentication path as completions.
func (p *HTTPProvider) Models(ctx context.Context) ([]ModelCard, error) {
	request, err := p.NewEndpointRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}

	response, err := p.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("llm: listing models: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		limit := p.MaxErrorBodyBytes
		if limit <= 0 {
			limit = defaultMaxErrorBody
		}

		excerpt, _ := io.ReadAll(io.LimitReader(response.Body, int64(limit)))

		return nil, fmt.Errorf("llm: model list returned %d: %s",
			response.StatusCode, strings.TrimSpace(string(excerpt)))
	}

	var payload struct {
		Data []ModelCard `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("llm: decoding model list: %w", err)
	}

	models := make([]ModelCard, 0, len(payload.Data))
	for _, model := range payload.Data {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID != "" {
			models = append(models, model)
		}
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("llm: the Gateway returned no enabled models")
	}

	return models, nil
}
