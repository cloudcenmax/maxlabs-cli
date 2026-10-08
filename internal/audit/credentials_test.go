package audit_test

import (
	"encoding/json"
	"strings"
	"testing"

	"censi/harness/internal/audit"
)

// TestCredentialMaterialIsRefused covers the one control the measurements
// support.
//
// A content detector caught roughly a tenth of injected instructions in the
// published benchmark. What survives is narrower and unconditional: a value that
// is unmistakably credential material must not be handed to something that can
// reach the network. That removes the exfiltration step rather than trying to
// out-guess the wording of an injection.
func TestCredentialMaterialIsRefused(t *testing.T) {
	secrets := map[string]string{
		"AWS key id":   "AKIAIOSFODNN7EXAMPLE",
		"GitHub token": "ghp_012345678901234567890123456789012345",
		"fine-grained": "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
		"API key":      "sk-proj-0123456789abcdefghijklmnop",
		"Google key":   "AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"Slack token":  "xoxb-0123456789-abcdefghijklmnop",
		"private key":  "-----BEGIN RSA PRIVATE KEY-----",
		"JWT":          "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"bearer":       "Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789",
	}

	for name, secret := range secrets {
		t.Run(name, func(t *testing.T) {
			arguments, _ := json.Marshal(map[string]string{"url": "https://example.test/?q=" + secret})

			decision := audit.Assess("webfetch", arguments, nil)

			if !decision.RefusedNow() {
				t.Fatalf("credential material was not refused: %s", decision.Explain())
			}

			// The value itself must not come back: repeating a secret is the
			// disclosure the refusal exists to prevent.
			if strings.Contains(decision.Explain(), secret) {
				t.Fatal("the refusal repeated the credential")
			}
		})
	}
}

// A credential passed to something that cannot leave is the operator's
// business, not this control's. Scanning everything would make the refusal
// common and therefore ignorable.
func TestALocalToolIsNotScanned(t *testing.T) {
	arguments := []byte(`{"path":"notes.txt","content":"AKIAIOSFODNN7EXAMPLE"}`)

	if audit.Assess("write", arguments, nil).RefusedNow() {
		t.Fatal("a local write was refused for containing a credential")
	}
}

// Ordinary code is full of long strings. A refusal that fires on them is one
// an operator turns off.
func TestOrdinaryTextIsNotRefused(t *testing.T) {
	benign := []string{
		`{"path":"internal/wire/wire.go"}`,
		`{"command":"go test ./..."}`,
		`{"url":"https://docs.example.com/guide"}`,
		// A git SHA and a base64 blob are both long and both fine.
		`{"url":"https://example.test/086f9a6f7a20a38935000064f964b371548d598be607f3ff0266366402e346a4"}`,
		`{"command":"echo aGVsbG8gd29ybGQgdGhpcyBpcyBub3QgYSBzZWNyZXQ="}`,
	}

	for _, arguments := range benign {
		if decision := audit.Assess("webfetch", []byte(arguments), nil); decision.RefusedNow() {
			t.Errorf("ordinary text was refused: %s", decision.Explain())
		}
	}
}

// Redaction says what was removed without repeating it.
func TestRedactionKeepsTheShapeAndDropsTheValue(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"

	redacted := audit.Redact("the key is " + secret + " and it ends here")

	if strings.Contains(redacted, secret) {
		t.Fatal("redaction left the credential in place")
	}

	if !strings.Contains(redacted, "the key is") || !strings.Contains(redacted, "and it ends here") {
		t.Fatalf("redaction removed more than the value: %q", redacted)
	}
}

// Text retrieved from a page is scanned too: a key can be carried in by a fetch
// and repeated out by the model.
func TestCredentialsInRetrievedTextAreFound(t *testing.T) {
	page := "Here is a config: AWS_KEY=AKIAIOSFODNN7EXAMPLE"

	if len(audit.FindCredentialText(page)) == 0 {
		t.Fatal("a credential in retrieved text was not found")
	}
}
