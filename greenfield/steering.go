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
