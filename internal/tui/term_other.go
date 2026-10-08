//go:build !darwin && !linux

package tui

import "os"

// On a platform without a raw-mode implementation the shell falls back to
// line-based input: history and Shift+Tab are unavailable, but everything else
// still works. Degrading is better than refusing to start.

type termState struct{}

func makeRaw(*os.File) (*termState, error) { return nil, errRawUnsupported }

func (s *termState) restore(*os.File) {}

// terminalWidth is unknown here, so callers fall back to a conventional default.
func terminalWidth(*os.File) int { return 0 }

// errRawUnsupported reports that this platform has no raw-mode support.
var errRawUnsupported = errNoRaw

type errNoRawType struct{}

func (errNoRawType) Error() string { return "tui: raw mode is not supported on this platform" }

var errNoRaw = errNoRawType{}
