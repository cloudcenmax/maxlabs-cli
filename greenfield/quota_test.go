package greenfield

import (
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/wire"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type quotaPauseProvider struct{ base flexibleProvider }

func (p *quotaPauseProvider) Complete(ctx context.Context, req wire.Request) (llm.Response, error) {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == session.BlockToolResult {
				return llm.Response{}, &llm.ServiceError{Code: "request_limit", Message: "Quota exhausted", Window: "weekly", CanRetry: false}
			}
		}
	}
	return p.base.Complete(ctx, req)
}
func TestQuotaPausePreservesBranchesChecksAndApprovalsForResume(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.Provider = &quotaPauseProvider{}
	_, _, err := r.RunApproved(context.Background())
	var quota *llm.ServiceError
	if !errors.As(err, &quota) || quota.Code != "request_limit" {
		t.Fatal("quota did not reach the host", err)
	}
	before, _ := s.Status(nil)
	paused := *before.Runs["alpha"]
	if paused.Status != "paused" || paused.Commit == "" || len(paused.Checks) == 0 || before.Runs["qa"] != nil {
		t.Fatal("quota lost preserved evidence", paused)
	}
	root, _ := s.Path(paused.Workspace)
	_ = os.WriteFile(filepath.Join(root, "setup-kept.txt"), []byte("keep setup"), 0600)
	r.Provider = &flexibleProvider{}
	if _, _, err = r.RunApproved(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Status(nil)
	resumed := after.Runs["alpha"]
	if resumed.Status != "passed" || resumed.Branch != paused.Branch || resumed.Workspace != paused.Workspace || resumed.Attempts != paused.Attempts || len(resumed.Checks) <= len(paused.Checks) || after.Approvals["spec"] != before.Approvals["spec"] {
		t.Fatal("resume restarted or lost evidence", resumed)
	}
	if data, err := os.ReadFile(filepath.Join(root, "setup-kept.txt")); err != nil || string(data) != "keep setup" {
		t.Fatal("setup was lost", err)
	}
}
