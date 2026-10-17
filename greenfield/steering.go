package greenfield

import "strings"

// DirectionReview transfers dispatch control back to the planner. The clients
// consume it as normal coordinator input rather than displaying an error.
type DirectionReview struct{ Directions []string }

func (d *DirectionReview) Error() string {
	return "Coordinator review requested for new user direction"
}
func DirectionReviewPrompt(directions []string) string {
	return "Process the new user direction received during approved Team execution as a normal coordinator request, not a task failure. Inspect team status and preserve completed work, checkpoints, budgets, and existing approvals. Scheduling preferences such as parallel execution do not by themselves change scope or invalidate approval: use the approved dependencies and ready task waves. Continue approved work with team run when appropriate. If the request changes outcomes or approved decisions, revise only affected artifacts and wait for the normal client approval gate. Never invent approval, bypass dependencies, or dispatch work the user asked to stop. Explain your decision briefly. User direction:\n" + strings.Join(directions, "\n")
}

// ExecutionReview lets the orchestrator diagnose a failed wave and dispatch
// in-scope repairs without treating a QA finding as a terminal client error.
type ExecutionReview struct{ Summary string }

func (e *ExecutionReview) Error() string { return e.Summary }
func ExecutionReviewPrompt(summary string) string {
	return "Review this recorded Team execution failure as the engineering orchestrator. Inspect team status and the affected task evidence. Keep completed work and current approvals. For QA findings with actual failing checks, use team repair with id set to the existing approved implementation task that owns the defect and qa_id set to the failed QA task. This runs a separate repair worker from the preserved QA branch, retaining regression tests; then use team run on the QA task for fresh independent verification. Repairs must stay within existing approved outcomes and must not weaken tests. Respect the eight-worker batch budget and three repair-round limit. Never claim a failing check passed. For source tampering, exhausted budgets, missing credentials, or genuine scope changes, explain the evidence and request only the decision actually needed. Do not dump raw wave JSON or ask to reapprove an unchanged spec merely because QA found a defect. Recorded failure:\n" + summary
}
