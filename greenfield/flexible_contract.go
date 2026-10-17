package greenfield

import (
	"fmt"
	"strings"
)

func flexible(state *State) bool { return state.ExecutionMode == "flexible" }

// Briefs remain durable coordination data; they do not restrict a worker's file choices.
func normalizeBrief(kind string, d map[string]any) {
	for _, key := range Contract.Kinds[kind] {
		if arrays[key] {
			if _, ok := d[key]; !ok {
				d[key] = []any{}
			}
		}
	}
	if kind == "task" {
		if text(d, "role") == "" {
			d["role"] = "implementation"
		}
	}
	if kind == "spec" {
		if _, ok := d["stack"]; !ok {
			d["stack"] = map[string]any{}
		}
		for i, v := range list(d, "acceptance") {
			if value, ok := v.(string); ok {
				list(d, "acceptance")[i] = map[string]any{"id": fmt.Sprintf("outcome-%d", i+1), "expect": value}
			}
		}
	}
}
func validateFlexible(kind, id string, d map[string]any) error {
	required, ok := Contract.FlexibleKinds[kind]
	if !ok || !slug.MatchString(id) {
		return fmt.Errorf("unknown brief kind or invalid ID")
	}
	for _, key := range required {
		if _, ok := d[key]; !ok {
			return fmt.Errorf("%s.%s is required", kind, key)
		}
	}
	if strings.TrimSpace(text(d, "summary")) == "" {
		return fmt.Errorf("summary must be nonempty")
	}
	for key := range arrays {
		if value, ok := d[key]; ok {
			if _, ok := value.([]any); !ok {
				return fmt.Errorf("%s must be an array", key)
			}
		}
	}
	if kind == "task" {
		if value, ok := d["serial"]; ok {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("serial must be a boolean")
			}
		}
		if len(list(d, "acceptance")) == 0 {
			return fmt.Errorf("describe at least one observable task outcome")
		}
		role := text(d, "role")
		if role != "foundation" && role != "implementation" && role != "qa" {
			return fmt.Errorf("invalid task role")
		}
		for _, v := range list(d, "depends_on") {
			if value, ok := v.(string); !ok || value == "" {
				return fmt.Errorf("dependencies must contain task IDs")
			}
		}
	}
	if kind == "design" && strings.TrimSpace(text(d, "prototype_html")) == "" {
		return fmt.Errorf("design needs a reviewable prototype_html")
	}
	return nil
}
func (s *Store) SetExecutionMode(mode string) (Status, error) {
	if mode != "flexible" && mode != "scoped" {
		return Status{}, fmt.Errorf("choose flexible or scoped execution")
	}
	var result Status
	err := s.Locked(func(state *State) error {
		if state.ExecutionMode != mode {
			state.ExecutionMode = mode
			delete(state.Approvals, "spec")
			delete(state.Approvals, "release")
			for _, run := range state.Runs {
				run.Status = "stale"
			}
			state.Revision++
			if err := s.Persist(state); err != nil {
				return err
			}
		}
		var err error
		result, err = s.Status(state)
		return err
	})
	return result, err
}
