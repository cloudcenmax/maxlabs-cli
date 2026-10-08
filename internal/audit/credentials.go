package audit

import (
	"regexp"
	"strings"
)

// credentialPatterns match material that must never leave the machine.
//
// Each has a fixed, vendor-assigned shape rather than being an entropy
// heuristic. That is deliberate: a general "looks random" rule fires on hashes,
// ids and base64 in ordinary code, and its false positives would land on a
// blocking path - which is how a guard becomes something an operator turns off.
//
// The cost of the narrow shapes is that an unrecognised secret passes. That is
// accepted: this catches the credential a tool was handed by accident, not a
// determined exfiltration by someone who already has code execution.
var credentialPatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"an AWS access key id", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a GitHub token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b`)},
	{"a GitHub fine-grained token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"an API key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"a JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{"a bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}=*`)},
}

// egressTools are the tools whose arguments leave the machine.
//
// Only these are scanned. A credential passed to a file write stays local and is
// the operator's business; the same string handed to a fetch or a shell that can
// reach the network is the exfiltration leg of an injection, and removing that
// leg is the one control the measurements support.
var egressTools = map[string]bool{
	"webfetch": true,
	"bash":     true,
	"task":     true,
}

// Credential is what was found and where.
type Credential struct {
	// Tool is the tool the value was being passed to.
	Tool string

	// What describes the kind of material, in words a person can act on. The
	// value itself is never returned, logged, or put back in front of the
	// model: repeating a secret is the disclosure this exists to prevent.
	What string
}

// FindCredentials reports credential material in a call's arguments.
//
// The scanned text is scanned as text, not parsed: a secret can arrive inside
// JSON, inside a URL query, or inside a shell command, and all three are the
// same string by the time a provider sees them.
func FindCredentials(tool string, arguments []byte) []Credential {
	if !egressTools[tool] {
		return nil
	}

	text := string(arguments)

	var found []Credential

	for _, candidate := range credentialPatterns {
		if candidate.pattern.MatchString(text) {
			found = append(found, Credential{Tool: tool, What: candidate.name})
		}
	}

	return dedupeCredentials(found)
}

// FindCredentialText reports credential material in free text.
//
// Used for retrieved content as well as arguments: a page can carry a key the
// model then repeats, and the second occurrence is the one that leaves.
func FindCredentialText(text string) []Credential {
	var found []Credential

	for _, candidate := range credentialPatterns {
		if candidate.pattern.MatchString(text) {
			found = append(found, Credential{What: candidate.name})
		}
	}

	return dedupeCredentials(found)
}

// dedupeCredentials removes repeats of the same kind.
//
// A shell command naming the same key twice is one disclosure, and reporting it
// twice tells a person nothing the first line did not.
func dedupeCredentials(found []Credential) []Credential {
	seen := make(map[string]bool, len(found))

	out := found[:0]

	for _, credential := range found {
		key := credential.Tool + "/" + credential.What

		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, credential)
	}

	return out
}

// Redact removes credential material from text.
//
// The pattern is replaced rather than the value, so what a person sees says
// what was removed without repeating it.
func Redact(text string) string {
	for _, candidate := range credentialPatterns {
		text = candidate.pattern.ReplaceAllString(text, "[redacted "+strings.TrimPrefix(candidate.name, "a ")+"]")
	}

	return text
}
