package greenfield

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
	"censi/harness/internal/wire"
)

type qaRepairProvider struct{}

func (p qaRepairProvider) Complete(ctx context.Context, req wire.Request) (llm.Response, error) {
	packet := map[string]any{}
	hasResult := false
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult {
				hasResult = true
			}
			if i := strings.LastIndex(b.Text, "\n\n{"); i >= 0 {
				_ = json.Unmarshal([]byte(b.Text[i+2:]), &packet)
			}
		}
	}
	task, _ := packet["task"].(map[string]any)
	id := text(task, "summary")
	qa := text(task, "role") == "qa"
	if hasResult {
		if qa && packet["previous_attempt"] == nil {
			return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text(`{"outcome":"blocked","summary":"QA reproduced the alpha outcome regression; repair implementation without weakening tests.","blockers":["alpha is wrong"]}`)}}}, nil
		}
		return completedProvider{}.Complete(ctx, req)
	}
	calls := []session.Block{}
	add := func(name string, args any) {
		raw, _ := json.Marshal(args)
		calls = append(calls, session.ToolCall(name, name, raw))
	}
	if qa {
		if packet["previous_attempt"] == nil {
			add("write", map[string]string{"path": "tests/qa.test.mjs", "content": "import assert from 'node:assert/strict'; import {value} from '../alpha.mjs'; assert.equal(value,'alpha');"})
		}
		add("verify", map[string]any{"command": "node --test acceptance.test.mjs tests/qa.test.mjs", "final": true})
	} else {
		value := id
		if id == "alpha" && packet["qa_repair"] == nil {
			value = "wrong"
		}
		add("write", map[string]string{"path": id + ".mjs", "content": "export const value='" + value + "';"})
		command := "node --check " + id + ".mjs"
		if packet["qa_repair"] != nil {
			command = "node --test acceptance.test.mjs tests/qa.test.mjs"
		}
		add("verify", map[string]any{"command": command, "final": true})
	}
	return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: calls}}, nil
}
func TestQAFindingRepairAndIndependentRecheck(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.Provider = qaRepairProvider{}
	_, _, err := r.RunApproved(context.Background())
	var review *ExecutionReview
	if !errors.As(err, &review) {
		t.Fatal("QA failure was not returned to orchestrator", err)
	}
	state, _ := s.Load()
	qa := state.Runs["qa"]
	if qa.StopReason != "qa_findings" || len(qa.QATestFiles) != 1 || qa.Commit == "" || qa.Checks[0].Passed {
		t.Fatal("QA regression not preserved", qa)
	}
	oldQA := qa.Workspace
	approval := state.Approvals["spec"]
	result, err := r.Execute(context.Background(), json.RawMessage(`{"action":"repair","id":"alpha","qa_id":"qa"}`))
	if err != nil || result.IsError {
		t.Fatal(err, resultText(result))
	}
	result, err = r.Execute(context.Background(), json.RawMessage(`{"action":"run","id":"qa"}`))
	if err != nil || result.IsError {
		t.Fatal(err, resultText(result))
	}
	state, _ = s.Load()
	if state.Runs["qa"].Status != "passed" || state.Runs["alpha"].Status != "passed" || state.Runs["qa"].Workspace == oldQA || state.Runs["qa"].QARepairRounds != 1 || state.Approvals["spec"] != approval {
		t.Fatal("repair did not receive fresh independent QA", state.Runs)
	}
	result, err = r.Execute(context.Background(), json.RawMessage(`{"action":"repair","id":"alpha","qa_id":"qa"}`))
	if err != nil || !result.IsError {
		t.Fatal("repair allowed without failing QA evidence", err, result)
	}
}
func TestQACanOnlyAddTestsAndCannotWeakenExistingFiles(t *testing.T) {
	before := map[string]string{"app.php": "one", "tests/existing.php": "original"}
	for _, path := range []string{"app.php", "tests/existing.php", "routes/web.php"} {
		after := map[string]string{}
		for p, v := range before {
			after[p] = v
		}
		after[path] = "changed"
		if valid, _ := qaSourceChanges(before, after); valid {
			t.Fatal("QA changed existing source", path)
		}
	}
	after := map[string]string{"app.php": "one", "tests/existing.php": "original", "tests/new.php": "regression"}
	if valid, added := qaSourceChanges(before, after); !valid || len(added) != 1 {
		t.Fatal(valid, added)
	}
}

func TestQARepairRejectsStaleSourceAndExhaustedRounds(t *testing.T) {
	for _, mode := range []string{"changed-source", "budget"} {
		t.Run(mode, func(t *testing.T) {
			s := flexibleSeed(t)
			r := runtime(t, s, false)
			r.Provider = qaRepairProvider{}
			_, _, _ = r.RunApproved(context.Background())
			state, _ := s.Load()
			if mode == "budget" {
				state.Runs["qa"].QARepairRounds = 3
				_ = s.Persist(&state)
			} else {
				root, _ := s.Path(state.Runs["qa"].Workspace)
				_ = os.WriteFile(filepath.Join(root, "alpha.mjs"), []byte("changed after QA"), 0600)
			}
			result, err := r.Execute(context.Background(), json.RawMessage(`{"action":"repair","id":"alpha","qa_id":"qa"}`))
			if err != nil || !result.IsError {
				t.Fatal("invalid repair permitted", err, result)
			}
		})
	}
}
func TestVerificationDoesNotHideEarlierSequenceFailure(t *testing.T) {
	ws, _ := tools.NewWorkspace(t.TempDir())
	recorded := true
	verify := verificationTool{shell: tools.NewBashTool(ws), record: func(_ string, r tools.Result, _ bool) error { recorded = !r.IsError; return nil }}
	result, err := verify.Execute(context.Background(), json.RawMessage(`{"command":"false; true","final":true}`))
	if err != nil || !result.IsError || recorded {
		t.Fatal("sequence hid a failed check", err, result)
	}
}
