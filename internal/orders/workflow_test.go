package orders

import "testing"

// The state machine is the piece of this system that must never be wrong, so it
// is pinned by an explicit table rather than left to be inferred from handler
// code.
func TestTransitionTable(t *testing.T) {
	all := []string{
		StatusPending, StatusAccepted, StatusPreparing,
		StatusReady, StatusCompleted, StatusCancelled,
	}
	allowed := map[string]map[string]bool{
		StatusPending:   {StatusAccepted: true, StatusCancelled: true},
		StatusAccepted:  {StatusPreparing: true, StatusCancelled: true},
		StatusPreparing: {StatusReady: true},
		StatusReady:     {StatusCompleted: true},
		StatusCompleted: {},
		StatusCancelled: {},
	}
	for _, from := range all {
		for _, to := range all {
			want := allowed[from][to]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatesAreTerminal(t *testing.T) {
	for _, terminal := range []string{StatusCompleted, StatusCancelled} {
		for _, to := range []string{StatusPending, StatusAccepted, StatusPreparing, StatusReady, StatusCompleted} {
			if CanTransition(terminal, to) {
				t.Errorf("%s must not be able to move to %s", terminal, to)
			}
		}
	}
}

func TestNoStateMayBeSkipped(t *testing.T) {
	// Every step of the happy path must be taken one at a time.
	path := []string{StatusPending, StatusAccepted, StatusPreparing, StatusReady, StatusCompleted}
	for i := 0; i < len(path)-1; i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Errorf("%s → %s should be allowed", path[i], path[i+1])
		}
		for j := i + 2; j < len(path); j++ {
			if CanTransition(path[i], path[j]) {
				t.Errorf("%s → %s should be rejected (skips steps)", path[i], path[j])
			}
		}
	}
}

func TestCancellationOnlyBeforePreparation(t *testing.T) {
	if !IsCancellable(StatusPending) {
		t.Error("a pending order should be cancellable")
	}
	if !IsCancellable(StatusAccepted) {
		t.Error("an accepted order should be cancellable")
	}
	for _, status := range []string{StatusPreparing, StatusReady, StatusCompleted, StatusCancelled} {
		if IsCancellable(status) {
			t.Errorf("an order in %s must not be cancellable", status)
		}
	}
}

func TestIsActive(t *testing.T) {
	active := map[string]bool{
		StatusPending: true, StatusAccepted: true, StatusPreparing: true, StatusReady: true,
		StatusCompleted: false, StatusCancelled: false,
	}
	for status, want := range active {
		if got := IsActive(status); got != want {
			t.Errorf("IsActive(%s) = %v, want %v", status, got, want)
		}
	}
}

func TestNextStatusesNeverIncludesUnsafeMoves(t *testing.T) {
	for _, status := range []string{StatusPending, StatusAccepted, StatusPreparing, StatusReady, StatusCompleted, StatusCancelled} {
		for _, next := range NextStatuses(status) {
			if !CanTransition(status, next) {
				t.Errorf("NextStatuses(%s) offered %s, which the table forbids", status, next)
			}
		}
	}
}

func TestTimelineMarksProgress(t *testing.T) {
	times := map[string]string{StatusPending: "t0", StatusAccepted: "t1"}
	steps := Timeline(StatusPreparing, times)
	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d", len(steps))
	}
	want := []string{"DONE", "DONE", "CURRENT", "UPCOMING", "UPCOMING"}
	for i, step := range steps {
		if step.State != want[i] {
			t.Errorf("step %d (%s) = %s, want %s", i, step.Key, step.State, want[i])
		}
	}
}

func TestTimelineForCancelledOrderShowsHowFarItGot(t *testing.T) {
	// Cancelled after being accepted: the customer should see the first two
	// steps ticked, not an empty timeline.
	times := map[string]string{StatusPending: "t0", StatusAccepted: "t1"}
	steps := Timeline(StatusCancelled, times)
	if steps[0].State != "DONE" || steps[1].State != "DONE" {
		t.Errorf("expected the reached steps to be done, got %+v", steps)
	}
	for _, step := range steps[2:] {
		if step.State == "DONE" || step.State == "CURRENT" {
			t.Errorf("step %s should be upcoming after cancellation, got %s", step.Key, step.State)
		}
	}
}

func TestTimelineForCancelledOrderAlwaysTicksTheFirstStep(t *testing.T) {
	// Even with no recorded history, the customer must see that the order was
	// placed before it was cancelled.
	steps := Timeline(StatusCancelled, map[string]string{})
	if len(steps) == 0 || steps[0].State == "UPCOMING" {
		t.Errorf("a cancelled order should still show where it stopped, got %+v", steps)
	}
	for _, step := range steps {
		if step.State == "CURRENT" {
			t.Errorf("a cancelled order has no live step, but %s is current", step.Key)
		}
	}
}

func TestStatusLabelCoversEveryState(t *testing.T) {
	for _, status := range []string{
		StatusPending, StatusAccepted, StatusPreparing,
		StatusReady, StatusCompleted, StatusCancelled,
	} {
		if StatusLabel(status) == status {
			t.Errorf("status %s has no customer-facing label", status)
		}
	}
}

func TestParseOrderNumberRejectsRubbish(t *testing.T) {
	valid := map[string]int32{"1": 1, "1024": 1024, "99999": 99999}
	for input, want := range valid {
		got, err := ParseOrderNumber(input)
		if err != nil || got != want {
			t.Errorf("ParseOrderNumber(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "-4", "abc", "1.5", "99999999", "1e3"} {
		if _, err := ParseOrderNumber(input); err == nil {
			t.Errorf("ParseOrderNumber(%q) should have failed", input)
		}
	}
}

func TestMaskPhoneHidesTheMiddle(t *testing.T) {
	if got := MaskPhone("+919876543210"); got != "+91••••3210" {
		t.Errorf("MaskPhone = %q", got)
	}
	// A short value is returned as-is rather than leaking a partial mask.
	if got := MaskPhone("123"); got != "123" {
		t.Errorf("MaskPhone = %q", got)
	}
}
