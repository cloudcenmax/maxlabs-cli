package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAccessTokenRefreshesAndPersistsExpiredSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != ClientID {
			t.Fatalf("form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh", "refresh_token": "rotated", "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "session.json")
	raw, _ := json.Marshal(Tokens{AccessToken: "stale", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute)})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	session := NewSession(server.URL, path)
	token, err := session.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "fresh" {
		t.Fatalf("token = %q", token)
	}

	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsJSONValue(persisted, "refresh_token", "rotated") {
		t.Fatalf("rotated refresh token was not persisted: %s", persisted)
	}
}

func TestStartDeviceGrantRequestsInferenceScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != ClientID {
			t.Fatalf("client_id = %q", r.Form.Get("client_id"))
		}
		if r.Form.Get("scope") != "profile usage:read chat" {
			t.Fatalf("scope = %q", r.Form.Get("scope"))
		}
		_ = json.NewEncoder(w).Encode(deviceGrant{
			DeviceCode: "device", UserCode: "ABCD-EFGH", VerificationURI: "https://example.test",
			ExpiresIn: 600, Interval: 5,
		})
	}))
	defer server.Close()

	grant, err := startDeviceGrant(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if grant.UserCode != "ABCD-EFGH" {
		t.Fatalf("user code = %q", grant.UserCode)
	}
}

func containsJSONValue(raw []byte, key, expected string) bool {
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return false
	}

	return values[key] == expected
}
