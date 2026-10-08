package agent

import (
	"strings"
	"sync"
)

// SteeringQueue carries user directions into a running turn.
//
// Add and Drain are safe across goroutines. CloseIfEmpty is the important
// final-boundary operation: it either hands the agent directions that arrived
// before completion, or atomically closes the queue so a late submission can
// be rejected and started as a new turn instead of being silently lost.
type SteeringQueue struct {
	mu     sync.Mutex
	items  []string
	closed bool
}

// NewSteeringQueue returns an open queue for one running turn.
func NewSteeringQueue() *SteeringQueue {
	return &SteeringQueue{}
}

// Add records a direction. False means the turn has already crossed its final
// safe boundary and the caller should submit the text as a new turn.
func (q *SteeringQueue) Add(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return false
	}

	q.items = append(q.items, text)

	return true
}

// Drain returns every direction received since the previous safe boundary.
func (q *SteeringQueue) Drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	items := append([]string(nil), q.items...)
	q.items = nil

	return items
}

// CloseIfEmpty atomically closes a completed turn when no direction is
// waiting. When it returns false, Drain will return work the agent must apply.
func (q *SteeringQueue) CloseIfEmpty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) > 0 {
		return false
	}

	q.closed = true

	return true
}

// Close rejects future directions when a turn stops on an error or interrupt.
func (q *SteeringQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
}
