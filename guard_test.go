package harness

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This harness is published as open source and must be neutral about both
// inference providers and their models. Naming either in the source would:
//
//   - tie a general mechanism to one vendor's behaviour, so the code reads as
//     vendor-specific when it is not;
//   - leak the routing topology and vendor selection of the deployment it was
//     extracted from;
//   - rot, because provider capabilities and model names change continuously.
//
// The rule is mechanical rather than cultural: the test below scans the tree for
// vendor names, model names, and references to the upstream implementation this
// harness learned from, and fails the build.
//
// If you are adding provider-specific behaviour, the correct home for it is the
// caller or the gateway that routes to the provider. Express cache behaviour
// here as a capability - implicit caching, explicit breakpoints, or none -
// never as a brand or a model id.
var forbidden = []struct {
	what string
	re   *regexp.Regexp
}{
	// -- inference providers and vendors -------------------------------------
	{"vendor", regexp.MustCompile(`(?i)\bdeep\s?seek\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bopen\s?router\b`)},
	{"vendor", regexp.MustCompile(`(?i)\banthropic\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bopenai\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bgoogle\s+(ai|deepmind|vertex)\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bmoonshot\s?ai\b`)},
	{"vendor", regexp.MustCompile(`(?i)\balibaba\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bsilicon\s?flow\b`)},
	{"vendor", regexp.MustCompile(`(?i)\btogether\s?ai\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bfireworks\s?ai\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bbedrock\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bazure\s+openai\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bvertex\s+ai\b`)},
	{"vendor", regexp.MustCompile(`(?i)\bz\.?ai\b`)},

	// -- model families -------------------------------------------------------
	{"model", regexp.MustCompile(`(?i)\bclaude\b`)},
	{"model", regexp.MustCompile(`(?i)\bsonnet\b`)},
	{"model", regexp.MustCompile(`(?i)\bopus\b`)},
	{"model", regexp.MustCompile(`(?i)\bhaiku\b`)},
	{"model", regexp.MustCompile(`(?i)\bgpt-?[0-9]`)},
	{"model", regexp.MustCompile(`(?i)\bgemini\b`)},
	{"model", regexp.MustCompile(`(?i)\bqwen\b`)},
	{"model", regexp.MustCompile(`(?i)\bkimi\b`)},
	{"model", regexp.MustCompile(`(?i)\bgrok\b`)},
	{"model", regexp.MustCompile(`(?i)\bllama\b`)},
	{"model", regexp.MustCompile(`(?i)\bmistral\b`)},
	{"model", regexp.MustCompile(`(?i)\bglm-[0-9]`)},
	{"model", regexp.MustCompile(`(?i)\bo[13]-?(mini|preview|pro)\b`)},
	{"model", regexp.MustCompile(`(?i)\bcommand-r\b`)},
	{"model", regexp.MustCompile(`(?i)\bphi-[0-9]`)},
	{"model", regexp.MustCompile(`(?i)-flash\b`)},
	{"model", regexp.MustCompile(`(?i)-v[0-9](\.[0-9])?-pro\b`)},

	// -- the upstream implementation this harness learned from ----------------
	{"upstream reference", regexp.MustCompile(`(?i)\bDSH\b`)},
	{"upstream reference", regexp.MustCompile(`(?i)deep\s?seek\s+harness`)},
	{"upstream reference", regexp.MustCompile(`(?i)\bpi-ai\b`)},
}

// guardFile is this file. It necessarily contains the patterns above, so it is
// excluded from its own scan.
const guardFile = "guard_test.go"

// siblingSurfaces are other shipping surfaces of the harness that sit outside
// this module but under the same open-source rule. The desktop app ships the
// same neutrality obligation as the CLI; a brand in its frontend or main.go is
// as much a leak as one in cli/**. Build output is skipped — a compiled binary
// is not a source leak. Each entry is a directory relative to the module root.
var siblingSurfaces = []string{"../desktop"}

// scannedExts are the text files the rule covers. A brand in a README is as
// much a leak as a brand in a comment.
var scannedExts = map[string]bool{
	".go": true, ".md": true, ".yaml": true, ".yml": true, ".json": true, ".toml": true,
}

// scanTree walks one root and appends offenders.
func scanTree(root string, forbidden []struct {
	what string
	re   *regexp.Regexp
}) ([]string, error) {
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch dir := d.Name(); dir {
			case "testdata", ".git":
				return fs.SkipDir
			case "node_modules", "vendor", "bin", "dist", "build":
				// Dependency trees and compiled output are not source.
				return fs.SkipDir
			}
			return nil
		}
		if !scannedExts[filepath.Ext(path)] || filepath.Base(path) == guardFile {
			return nil
		}

		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(src), "\n") {
			for _, f := range forbidden {
				if loc := f.re.FindStringIndex(line); loc != nil {
					offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+
						" ["+f.what+": "+line[loc[0]:loc[1]]+"]: "+strings.TrimSpace(line))
					break
				}
			}
		}
		return nil
	})
	return offenders, err
}

// TestNoVendorOrModelNames fails if a provider name, a model name, or a
// reference to the upstream implementation appears anywhere in the module, or
// in a sibling shipping surface (see siblingSurfaces).
func TestNoVendorOrModelNames(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	var offenders []string
	for _, surface := range append([]string{root}, siblingSurfaces...) {
		found, scanErr := scanTree(surface, forbidden)
		if scanErr != nil {
			t.Fatalf("walk %s: %v", surface, scanErr)
		}
		offenders = append(offenders, found...)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("the harness must stay provider- and model-neutral, but %d line(s) name one:\n\n%s\n\n"+
			"Move provider-specific behaviour to the caller or the routing gateway, and express "+
			"cache behaviour here as a capability rather than a brand or a model id.",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

// TestSourcesParse keeps the guard honest: a file the guard cannot parse is a
// file it cannot protect.
func TestSourcesParse(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		if _, perr := parser.ParseFile(fset, path, nil, parser.PackageClauseOnly); perr != nil {
			t.Errorf("parse %s: %v", path, perr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
