package main

import "testing"

func wr(id int64, status, conclusion, created string) ghWorkflowRun {
	return ghWorkflowRun{ID: id, Status: status, Conclusion: conclusion, CreatedAt: created}
}

// labda-dev-a, fourth attempt: the instance started at 16:39:46 and the only
// run on the reused branch was the THIRD attempt's failure. Judging it ended
// the watch at poll 1/540 before this attempt's own run existed.
func TestPickRunIgnoresEarlierAttempt(t *testing.T) {
	runs := []ghWorkflowRun{wr(34376449958, "completed", "failure", "2026-09-09T16:24:02Z")}
	if got := pickRun(runs, "2026-09-09T16:39:46Z"); got != nil {
		t.Fatalf("judged an earlier attempt's run: %+v", *got)
	}
}

func TestPickRunTakesOwnRunAfterFloor(t *testing.T) {
	runs := []ghWorkflowRun{
		wr(34378195260, "in_progress", "", "2026-09-09T16:40:28Z"),
		wr(34376449958, "completed", "failure", "2026-09-09T16:24:02Z"),
	}
	got := pickRun(runs, "2026-09-09T16:39:46Z")
	if got == nil || got.ID != 34378195260 {
		t.Fatalf("did not pick this attempt's run: %+v", got)
	}
}

// The pipeline's own secrets commit cancels its in-flight run. That is not a
// verdict: before the successor registers, keep polling; after, take it.
func TestPickRunCancelledIsNotAVerdict(t *testing.T) {
	floor := "2026-09-09T16:39:46Z"
	only := []ghWorkflowRun{wr(34378195260, "completed", "cancelled", "2026-09-09T16:40:28Z")}
	if got := pickRun(only, floor); got != nil {
		t.Fatalf("treated a self-cancelled run as the result: %+v", *got)
	}
	both := []ghWorkflowRun{
		wr(34379635567, "in_progress", "", "2026-09-09T16:54:22Z"),
		wr(34378195260, "completed", "cancelled", "2026-09-09T16:40:28Z"),
	}
	if got := pickRun(both, floor); got == nil || got.ID != 34379635567 {
		t.Fatalf("did not move on to the successor run: %+v", got)
	}
}

func TestPickRunSkippedIsNotAVerdict(t *testing.T) {
	runs := []ghWorkflowRun{wr(1, "completed", "skipped", "2026-09-09T16:40:30Z")}
	if got := pickRun(runs, "2026-09-09T16:39:46Z"); got != nil {
		t.Fatalf("judged a skipped run: %+v", *got)
	}
}

// Without a floor (an input from before this field existed) the newest real
// run is taken, as before.
func TestPickRunWithoutFloor(t *testing.T) {
	runs := []ghWorkflowRun{wr(7, "completed", "failure", "2026-09-08T10:00:00Z")}
	if got := pickRun(runs, ""); got == nil || got.ID != 7 {
		t.Fatalf("no-floor behaviour changed: %+v", got)
	}
}

// A run whose creation time cannot be read cannot be shown to be this
// attempt's, so it is not judged.
func TestPickRunUnreadableCreatedAt(t *testing.T) {
	runs := []ghWorkflowRun{wr(9, "completed", "success", "")}
	if got := pickRun(runs, "2026-09-09T16:39:46Z"); got != nil {
		t.Fatalf("judged a run of unknown age: %+v", *got)
	}
}

// The cluster clock runs ahead of GitHub's: this attempt's own run reports a
// created_at 16 s before the instance started. It is still this attempt's run.
func TestPickRunToleratesClockSkew(t *testing.T) {
	runs := []ghWorkflowRun{wr(34378195260, "in_progress", "", "2026-09-09T16:39:30Z")}
	if got := pickRun(runs, "2026-09-09T16:39:46Z"); got == nil || got.ID != 34378195260 {
		t.Fatalf("discarded this attempt's run over 16s of clock skew: %+v", got)
	}
}

// The tolerance is a margin, not a hole: a run from minutes earlier is still an
// earlier attempt's.
func TestPickRunToleranceDoesNotReadmitEarlierAttempt(t *testing.T) {
	runs := []ghWorkflowRun{wr(34376449958, "completed", "failure", "2026-09-09T16:38:00Z")}
	if got := pickRun(runs, "2026-09-09T16:39:46Z"); got != nil {
		t.Fatalf("tolerance readmitted a run from 106s before the start: %+v", *got)
	}
}
