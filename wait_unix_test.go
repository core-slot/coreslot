//go:build unix

package coreslot

import (
	"context"
	"testing"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/engine"
)

// dueStepper is a ticket that is never admitted and whose next work is always due in a second.
type dueStepper struct{ steps int }

func (s *dueStepper) Step(context.Context) (engine.StepResult, error) {
	s.steps++
	return engine.Waiting{Position: 2}, nil
}

func (s *dueStepper) Due() time.Duration { return time.Second }

// Wait Steps when the ticket's next work is due, not on a fixed fast poll: in 1.5 s, a ticket
// whose work is due every second is Stepped at 0 s and 1 s.
func TestWaitStepsWhenDue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s := &dueStepper{}
	if r, err := stepWhenDue(ctx, s); r != nil || err != nil {
		t.Fatalf("stepWhenDue = %v, %v; want nil, nil at the context's end", r, err)
	}
	if s.steps != 2 {
		t.Fatalf("%d Steps in 1.5 s with work due every second, want 2", s.steps)
	}
}
