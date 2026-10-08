package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const ClientID = "cli"

var ErrNotSignedIn = errors.New("not signed in")

type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type Session struct {
	baseURL string
	path    string
	mu      sync.Mutex
	tokens  Tokens
	loaded  bool
}

func NewSession(baseURL, path string) *Session {
	return &Session{baseURL: strings.TrimRight(baseURL, "/"), path: path}
}

func (s *Session) SignedIn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.loadLocked() == nil && s.tokens.AccessToken != ""
}

func (s *Session) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.loadLocked(); err != nil {
		return "", err
	}
	if s.tokens.AccessToken == "" {
		return "", ErrNotSignedIn
	}
	if !s.tokens.ExpiresAt.IsZero() && time.Now().Add(time.Minute).Before(s.tokens.ExpiresAt) {
		return s.tokens.AccessToken, nil
	}
	if s.tokens.RefreshToken == "" {
		return "", ErrNotSignedIn
	}

	tokens, err := exchange(ctx, s.baseURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {s.tokens.RefreshToken},
	})
	if err != nil {
		return "", err
	}
	if err := s.saveLocked(tokens); err != nil {
		return "", err
	}

	return tokens.AccessToken, nil
}

func (s *Session) LoginDevice(ctx context.Context, output io.Writer) error {
	grant, err := startDeviceGrant(ctx, s.baseURL)
	if err != nil {
		return err
	}

	verificationURL := grant.VerificationURIComplete
	if verificationURL == "" {
		verificationURL = grant.VerificationURI
	}
	_, _ = fmt.Fprintf(output, "Open %s and approve code %s\n", verificationURL, grant.UserCode)

	interval := time.Duration(max(grant.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(grant.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}

		tokens, state, err := pollDeviceGrant(ctx, s.baseURL, grant.DeviceCode)
		if err != nil {
			return err
		}

		switch state {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied":
			return errors.New("authorization was denied")
		case "expired_token":
			return errors.New("the device code expired")
		}

		s.mu.Lock()
		err = s.saveLocked(tokens)
		s.mu.Unlock()

		return err
	}

	return errors.New("the device code expired")
}

func (s *Session) loadLocked() error {
	if s.loaded {
		return nil
	}
	s.loaded = true

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading OAuth session: %w", err)
	}
	if err := json.Unmarshal(raw, &s.tokens); err != nil {
		return fmt.Errorf("parsing OAuth session: %w", err)
	}

	return nil
}

func (s *Session) saveLocked(tokens Tokens) error {
	raw, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("encoding OAuth session: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("creating OAuth session directory: %w", err)
	}
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		return fmt.Errorf("writing OAuth session: %w", err)
	}
	s.tokens = tokens
	s.loaded = true

	return nil
}

type deviceGrant struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

func startDeviceGrant(ctx context.Context, baseURL string) (deviceGrant, error) {
	response, err := postForm(ctx, baseURL, "/oauth/device/code", url.Values{
		"client_id": {ClientID},
		"scope":     {"profile usage:read chat"},
	})
	if err != nil {
		return deviceGrant{}, err
	}
	defer func() { _ = response.Body.Close() }()

	var grant deviceGrant
	if err := json.NewDecoder(response.Body).Decode(&grant); err != nil {
		return deviceGrant{}, fmt.Errorf("reading device authorization: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return deviceGrant{}, fmt.Errorf("the gateway returned %d", response.StatusCode)
	}

	return grant, nil
}

func pollDeviceGrant(ctx context.Context, baseURL, deviceCode string) (Tokens, string, error) {
	response, err := postForm(ctx, baseURL, "/oauth/token", url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {ClientID},
		"device_code": {deviceCode},
	})
	if err != nil {
		return Tokens{}, "", err
	}
	defer func() { _ = response.Body.Close() }()

	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Tokens{}, "", fmt.Errorf("reading OAuth token response: %w", err)
	}
	if payload.Error != "" {
		return Tokens{}, payload.Error, nil
	}
	if payload.AccessToken == "" {
		return Tokens{}, "", errors.New("the gateway returned no access token")
	}

	return Tokens{
		AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken,
		TokenType: payload.TokenType, ExpiresAt: time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, "", nil
}

func exchange(ctx context.Context, baseURL string, form url.Values) (Tokens, error) {
	response, err := postForm(ctx, baseURL, "/oauth/token", form)
	if err != nil {
		return Tokens{}, err
	}
	defer func() { _ = response.Body.Close() }()

	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Tokens{}, fmt.Errorf("reading OAuth refresh response: %w", err)
	}
	if payload.Error != "" || payload.AccessToken == "" {
		return Tokens{}, fmt.Errorf("refreshing OAuth session failed: %s", payload.Error)
	}

	return Tokens{
		AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken,
		TokenType: payload.TokenType, ExpiresAt: time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}

func postForm(ctx context.Context, baseURL, path string, form url.Values) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("building OAuth request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("reaching OAuth gateway: %w", err)
	}
	if response.StatusCode >= 500 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("the OAuth gateway returned %d", response.StatusCode)
	}

	return response, nil
}
