package greenfield

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func compactCheckOutput(raw string) string {
	var payload map[string]any
	if json.NewDecoder(strings.NewReader(raw)).Decode(&payload) != nil || payload["tool"] != "phpunit" {
		return truncate(raw, 8000)
	}
	if failures, ok := payload["failures"].([]any); ok {
		for _, value := range failures {
			if failure, ok := value.(map[string]any); ok {
				if message, ok := failure["message"].(string); ok {
					if i := strings.Index(message, "\nStack trace:"); i >= 0 {
						message = message[:i]
					}
					if len(message) > 1200 {
						message = message[:500] + "\n[body omitted; see log]\n" + message[len(message)-650:]
					}
					failure["message"] = message
				}
			}
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return truncate(raw, 8000)
	}
	return truncate(string(out), 8000)
}
func (s *Store) recordCheck(id string, attempt, index int, command, raw string, passed bool) (Check, error) {
	relative := fmt.Sprintf("checks/%s-%d-%d.log", id, attempt, index)
	path, err := s.Path(relative)
	if err != nil {
		return Check{}, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return Check{}, err
	}
	if err = os.WriteFile(path, []byte(truncate(raw, 200000)), 0600); err != nil {
		return Check{}, err
	}
	return Check{Command: command, Passed: passed, Output: compactCheckOutput(raw), Log: ".maxlabs/team/" + relative}, nil
}

func (s *Store) archiveRun(id string, run *Run) error {
	relative := fmt.Sprintf("runs/%s-%s.json", id, run.ID)
	path, err := s.Path(relative)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	run.Archive = ".maxlabs/team/" + relative
	raw, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
