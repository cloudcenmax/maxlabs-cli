package main

import "testing"

func TestShouldLoginOAuth(t *testing.T) {
	tests := []struct {
		name       string
		authURL    string
		oauthLogin bool
		apiKey     string
		signedIn   bool
		want       bool
	}{
		{
			name:       "first run starts device login",
			authURL:    "http://gateway.test",
			oauthLogin: true,
			want:       true,
		},
		{
			name:       "saved session is reused",
			authURL:    "http://gateway.test",
			oauthLogin: true,
			signedIn:   true,
			want:       false,
		},
		{
			name:       "explicit API key fallback skips device login",
			authURL:    "http://gateway.test",
			oauthLogin: false,
			apiKey:     "key",
			want:       false,
		},
		{
			name:       "missing fallback still requires login",
			authURL:    "http://gateway.test",
			oauthLogin: false,
			want:       true,
		},
		{
			name:       "OAuth disabled skips device login",
			authURL:    "",
			oauthLogin: true,
			want:       false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := shouldLoginOAuth(test.authURL, test.oauthLogin, test.apiKey, test.signedIn)
			if got != test.want {
				t.Fatalf("shouldLoginOAuth() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestNormaliseWebSearch(t *testing.T) {
	if got := normaliseWebSearch("off"); got != "" {
		t.Fatalf("off normalised to %q", got)
	}
	if got := normaliseWebSearch("auto"); got != "auto" {
		t.Fatalf("auto normalised to %q", got)
	}
}
