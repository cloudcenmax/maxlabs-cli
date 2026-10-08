package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"censi/harness/internal/llm"
)

func TestModelsUsesTheAuthenticatedGatewayEndpoint(t *testing.T) {
	var path string
	var authorization string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authorization = r.Header.Get("Authorization")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"worker","description":"Fast","context_window":64000,"max_output_tokens":8192},{"id":"pro","description":"Deep","context_window":128000,"max_output_tokens":32768}]}`)
	}))
	defer server.Close()

	provider := &llm.HTTPProvider{BaseURL: server.URL, APIKey: "test-key"}
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}

	if path != "/models" {
		t.Fatalf("path = %q, want /models", path)
	}
	if authorization != "Bearer test-key" {
		t.Fatalf("authorization = %q", authorization)
	}
	if len(models) != 2 || models[1].ID != "pro" || models[1].MaxOutputTokens != 32768 {
		t.Fatalf("models = %+v", models)
	}
}

func TestModelsRejectsAnEmptyCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	defer server.Close()

	provider := &llm.HTTPProvider{BaseURL: server.URL}
	if _, err := provider.Models(context.Background()); err == nil {
		t.Fatal("an empty model catalog must be reported")
	}
}
