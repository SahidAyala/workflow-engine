package executor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/SahidAyala/Nocturn-Atlas-Workflow-Engine/internal/app/ports"
)

// discardLogger suppresses log output so test runs stay quiet; the executor
// logs a lot (by design — see docs/known-limitations.md's structured logging
// note) and none of that is worth asserting on here.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type markCall struct {
	id     string
	output []byte
}

type retryCall struct {
	id        string
	nextRunAt time.Time
	attempt   int
}

// fakeStepRunRepository is an in-memory ports.StepRunRepository test double.
// Every field is a canned response; every Mark*/Retry* call is recorded so
// tests can assert exactly which side effects processOne triggered.
type fakeStepRunRepository struct {
	next    *ports.PendingStepRun
	nextErr error

	projectID  string
	tenantID   string
	projectErr error

	pending    bool
	pendingErr error

	prevOutputs    []ports.StepIndexOutput
	prevOutputsErr error

	total, succeeded int
	countsErr        error

	markRunningErr   error
	markSucceededErr error
	markFailedErr    error
	retryErr         error

	markRunningCalls     []string
	markSucceededCalls   []markCall
	markFailedCalls      []markCall
	markWFRunningCalls   []string
	markWFSucceededCalls []string
	markWFFailedCalls    []string
	retryCalls           []retryCall
}

func (f *fakeStepRunRepository) GetNextPendingStepRun(context.Context) (*ports.PendingStepRun, error) {
	return f.next, f.nextErr
}

func (f *fakeStepRunRepository) GetPreviousStepOutputs(context.Context, string) ([]ports.StepIndexOutput, error) {
	return f.prevOutputs, f.prevOutputsErr
}

func (f *fakeStepRunRepository) IsWorkflowRunPending(context.Context, string) (bool, error) {
	return f.pending, f.pendingErr
}

func (f *fakeStepRunRepository) WorkflowRunStepCounts(context.Context, string) (int, int, error) {
	return f.total, f.succeeded, f.countsErr
}

func (f *fakeStepRunRepository) MarkStepRunRunning(_ context.Context, id string) error {
	f.markRunningCalls = append(f.markRunningCalls, id)
	return f.markRunningErr
}

func (f *fakeStepRunRepository) MarkStepRunSucceeded(_ context.Context, id string, output []byte) error {
	f.markSucceededCalls = append(f.markSucceededCalls, markCall{id, output})
	return f.markSucceededErr
}

func (f *fakeStepRunRepository) MarkStepRunFailed(_ context.Context, id string, output []byte) error {
	f.markFailedCalls = append(f.markFailedCalls, markCall{id, output})
	return f.markFailedErr
}

func (f *fakeStepRunRepository) MarkWorkflowRunRunning(_ context.Context, id string) error {
	f.markWFRunningCalls = append(f.markWFRunningCalls, id)
	return nil
}

func (f *fakeStepRunRepository) MarkWorkflowRunSucceeded(_ context.Context, id string) error {
	f.markWFSucceededCalls = append(f.markWFSucceededCalls, id)
	return nil
}

func (f *fakeStepRunRepository) MarkWorkflowRunFailed(_ context.Context, id string) error {
	f.markWFFailedCalls = append(f.markWFFailedCalls, id)
	return nil
}

func (f *fakeStepRunRepository) RetryStepRun(_ context.Context, id string, nextRunAt time.Time, attempt int) error {
	f.retryCalls = append(f.retryCalls, retryCall{id, nextRunAt, attempt})
	return f.retryErr
}

func (f *fakeStepRunRepository) GetProjectContext(context.Context, string) (string, string, error) {
	return f.projectID, f.tenantID, f.projectErr
}

// fakePublisher records every published event. If err is set, Publish always
// fails — used to prove publish failures are best-effort and never block the
// executor (see publishEvent's doc comment).
type fakePublisher struct {
	events []ports.WorkflowEvent
	err    error
}

func (p *fakePublisher) Publish(_ context.Context, e ports.WorkflowEvent) error {
	p.events = append(p.events, e)
	return p.err
}

func eventTypes(events []ports.WorkflowEvent) []ports.WorkflowEventType {
	out := make([]ports.WorkflowEventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func TestProcessOne(t *testing.T) {
	t.Run("no pending step: returns false, no side effects", func(t *testing.T) {
		repo := &fakeStepRunRepository{next: nil}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		if found {
			t.Error("expected found=false")
		}
		if len(repo.markRunningCalls) != 0 || len(pub.events) != 0 {
			t.Error("expected no repository or publisher calls")
		}
	})

	t.Run("GetNextPendingStepRun error: returns false, no side effects", func(t *testing.T) {
		repo := &fakeStepRunRepository{nextErr: errTest}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		if found {
			t.Error("expected found=false")
		}
		if len(pub.events) != 0 {
			t.Error("expected no events published")
		}
	})

	t.Run("first step of a run succeeds and completes the run", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-1", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 0, Attempt: 1, StepType: "delay",
				Config: []byte(`{"seconds":0}`),
			},
			pending: true,            // first step: run has no running/completed steps yet
			total:   1, succeeded: 1, // this step's success completes the run
		}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.markRunningCalls) != 1 || repo.markRunningCalls[0] != "step-1" {
			t.Errorf("markRunningCalls = %v", repo.markRunningCalls)
		}
		if len(repo.markWFRunningCalls) != 1 || repo.markWFRunningCalls[0] != "run-1" {
			t.Errorf("markWFRunningCalls = %v, want [run-1] (first step must mark the run running)", repo.markWFRunningCalls)
		}
		if len(repo.markSucceededCalls) != 1 || repo.markSucceededCalls[0].id != "step-1" {
			t.Errorf("markSucceededCalls = %v", repo.markSucceededCalls)
		}
		if len(repo.markWFSucceededCalls) != 1 || repo.markWFSucceededCalls[0] != "run-1" {
			t.Errorf("markWFSucceededCalls = %v, want [run-1] (last step must complete the run)", repo.markWFSucceededCalls)
		}
		if len(repo.markFailedCalls) != 0 || len(repo.retryCalls) != 0 {
			t.Error("expected no failure/retry calls on a success path")
		}
		want := []ports.WorkflowEventType{
			ports.StepRunStarted, ports.WorkflowRunStarted, ports.StepRunSucceeded, ports.WorkflowRunCompleted,
		}
		if got := eventTypes(pub.events); !eventTypesEqual(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
	})

	t.Run("a middle step succeeds without starting or completing the run", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-2", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 1, Attempt: 1, StepType: "delay",
				Config: []byte(`{"seconds":0}`),
			},
			pending: false,           // NOT the first step
			total:   3, succeeded: 2, // one more step still pending after this
		}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.markWFRunningCalls) != 0 {
			t.Error("expected MarkWorkflowRunRunning NOT called for a non-first step")
		}
		if len(repo.markWFSucceededCalls) != 0 {
			t.Error("expected MarkWorkflowRunSucceeded NOT called when steps remain")
		}
		want := []ports.WorkflowEventType{ports.StepRunStarted, ports.StepRunSucceeded}
		if got := eventTypes(pub.events); !eventTypesEqual(got, want) {
			t.Errorf("events = %v, want %v (no run-level events)", got, want)
		}
	})

	t.Run("retriable failure with attempts remaining schedules a retry, does not fail the step or run", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-1", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 0, Attempt: 1, StepType: "http_request",
				// Malformed URL -> a plain url.Parse error (retriable: not an
				// HTTPStatusError, not context.Canceled, not one of the
				// executor's own non-retriable sentinel error types).
				Config: []byte(`{"method":"GET","url":"://bad","retry":{"max_attempts":3,"backoff_seconds":1}}`),
			},
			pending: false, // isolate retry behavior from the first-step-start events
		}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.retryCalls) != 1 {
			t.Fatalf("retryCalls = %v, want exactly 1", repo.retryCalls)
		}
		if repo.retryCalls[0].id != "step-1" || repo.retryCalls[0].attempt != 2 {
			t.Errorf("retryCalls[0] = %+v, want {id: step-1, attempt: 2}", repo.retryCalls[0])
		}
		if !repo.retryCalls[0].nextRunAt.After(time.Now()) {
			t.Error("expected nextRunAt to be scheduled in the future")
		}
		if len(repo.markFailedCalls) != 0 || len(repo.markWFFailedCalls) != 0 {
			t.Error("expected no failure calls while a retry is still available")
		}
		// Only the start event — no failure/completion events on a scheduled retry.
		want := []ports.WorkflowEventType{ports.StepRunStarted}
		if got := eventTypes(pub.events); !eventTypesEqual(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
	})

	t.Run("retriable failure with attempts exhausted fails the step and the run", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-1", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 0, Attempt: 3, StepType: "http_request",
				Config: []byte(`{"method":"GET","url":"://bad","retry":{"max_attempts":3,"backoff_seconds":1}}`),
			},
			pending: false, // isolate retry-exhaustion behavior from the first-step-start events
		}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.retryCalls) != 0 {
			t.Errorf("retryCalls = %v, want none (attempts exhausted)", repo.retryCalls)
		}
		if len(repo.markFailedCalls) != 1 || repo.markFailedCalls[0].id != "step-1" {
			t.Errorf("markFailedCalls = %v", repo.markFailedCalls)
		}
		if len(repo.markWFFailedCalls) != 1 || repo.markWFFailedCalls[0] != "run-1" {
			t.Errorf("markWFFailedCalls = %v", repo.markWFFailedCalls)
		}
		want := []ports.WorkflowEventType{ports.StepRunStarted, ports.StepRunFailed, ports.WorkflowRunFailed}
		if got := eventTypes(pub.events); !eventTypesEqual(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
	})

	t.Run("non-retriable failure fails immediately even with attempts remaining", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-1", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 0, Attempt: 1, StepType: "not_a_real_step_type",
				// A generous retry budget that must be ignored: an unknown step
				// type is non-retriable regardless of attempts remaining.
				Config: []byte(`{"retry":{"max_attempts":10,"backoff_seconds":1}}`),
			},
			pending: true,
		}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.retryCalls) != 0 {
			t.Errorf("retryCalls = %v, want none (non-retriable error)", repo.retryCalls)
		}
		if len(repo.markFailedCalls) != 1 {
			t.Errorf("markFailedCalls = %v, want exactly 1", repo.markFailedCalls)
		}
		if len(repo.markWFFailedCalls) != 1 {
			t.Errorf("markWFFailedCalls = %v, want exactly 1", repo.markWFFailedCalls)
		}
	})

	t.Run("interpolation failure leaves the step untouched (no marks, no events)", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-2", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 1, Attempt: 1, StepType: "delay",
				Config: []byte(`{"seconds":"{{steps.0.output.x}}"}`),
			},
			pending: false,
			prevOutputs: []ports.StepIndexOutput{
				// Invalid JSON for step 0's stored output -> interpolateStepConfig
				// must error out before the step is ever claimed/run.
				{StepIndex: 0, Output: []byte(`not json`)},
			},
		}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		// processOne still reports "found work" (a step existed) even though it
		// could not be run — this documents current behavior: the step is left
		// pending forever on a bad interpolation, matching what the executor
		// actually does today (see docs/known-limitations.md if this should
		// change to a hard failure instead).
		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.markRunningCalls) != 0 {
			t.Error("expected MarkStepRunRunning NOT called when interpolation fails")
		}
		if len(pub.events) != 0 {
			t.Error("expected no events published when interpolation fails")
		}
	})

	t.Run("publish failures are logged but never block marking the step succeeded", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-1", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 0, Attempt: 1, StepType: "delay",
				Config: []byte(`{"seconds":0}`),
			},
			pending: true, total: 1, succeeded: 1,
		}
		pub := &fakePublisher{err: errTest} // every Publish call fails

		found := processOne(context.Background(), repo, pub, discardLogger())

		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.markSucceededCalls) != 1 {
			t.Error("expected the step to still be marked succeeded despite publish failures")
		}
		if len(pub.events) == 0 {
			t.Error("expected Publish to have been attempted (and recorded) even though it errors")
		}
	})

	t.Run("works with a nil publisher (publishing is optional)", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-1", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 0, Attempt: 1, StepType: "delay",
				Config: []byte(`{"seconds":0}`),
			},
			pending: true, total: 1, succeeded: 1,
		}

		var found bool
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("processOne panicked with a nil publisher: %v", r)
				}
			}()
			found = processOne(context.Background(), repo, nil, discardLogger())
		}()

		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.markSucceededCalls) != 1 {
			t.Error("expected the step to be marked succeeded")
		}
	})

	t.Run("a GetProjectContext error is non-fatal: the step still runs", func(t *testing.T) {
		repo := &fakeStepRunRepository{
			next: &ports.PendingStepRun{
				ID: "step-1", WorkflowRunID: "run-1", WorkflowID: "wf-1",
				StepIndex: 0, Attempt: 1, StepType: "delay",
				Config: []byte(`{"seconds":0}`),
			},
			pending: true, total: 1, succeeded: 1,
			projectErr: errTest,
		}
		pub := &fakePublisher{}

		found := processOne(context.Background(), repo, pub, discardLogger())

		if !found {
			t.Fatal("expected found=true")
		}
		if len(repo.markSucceededCalls) != 1 {
			t.Error("expected the step to still succeed despite the project-context lookup failing")
		}
	})
}

func eventTypesEqual(a, b []ports.WorkflowEventType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var errTest = testError("boom")

type testError string

func (e testError) Error() string { return string(e) }

func TestNextPollDelay(t *testing.T) {
	t.Run("resets to base and adds jitter when work was found", func(t *testing.T) {
		idle := pollBackoffMax // simulate having backed all the way off
		got := nextPollDelay(true, &idle)

		if idle != pollIntervalBase {
			t.Errorf("idleDelay = %v, want reset to %v", idle, pollIntervalBase)
		}
		if got < pollIntervalBase || got > pollIntervalBase+pollJitter {
			t.Errorf("got %v, want within [%v, %v]", got, pollIntervalBase, pollIntervalBase+pollJitter)
		}
	})

	t.Run("backs off by 1.5x per empty poll, capped at pollBackoffMax", func(t *testing.T) {
		idle := pollIntervalBase
		for i := 0; i < 30; i++ {
			got := nextPollDelay(false, &idle)
			if idle > pollBackoffMax {
				t.Fatalf("idleDelay exceeded cap: %v > %v", idle, pollBackoffMax)
			}
			if got < idle || got > idle+pollJitter {
				t.Fatalf("got %v, want within [%v, %v]", got, idle, idle+pollJitter)
			}
		}
		if idle != pollBackoffMax {
			t.Errorf("idleDelay = %v, want to have converged to the cap %v", idle, pollBackoffMax)
		}
	})
}

func TestParseRetryPolicy(t *testing.T) {
	tests := []struct {
		name               string
		config             []byte
		wantMaxAttempts    int
		wantBackoffSeconds int
	}{
		{"empty config: single try, no backoff", nil, 1, 0},
		{"no retry field: single try, no backoff", []byte(`{"url":"x"}`), 1, 0},
		{"invalid JSON: falls back to single try, no backoff", []byte(`not json`), 1, 0},
		{
			"explicit retry policy is honored",
			[]byte(`{"retry":{"max_attempts":5,"backoff_seconds":10}}`),
			5, 10,
		},
		{
			"zero max_attempts falls back to 1 (not 0)",
			[]byte(`{"retry":{"max_attempts":0,"backoff_seconds":10}}`),
			1, 10,
		},
		{
			"negative backoff_seconds is clamped to 0",
			[]byte(`{"retry":{"max_attempts":3,"backoff_seconds":-5}}`),
			3, 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMax, gotBackoff := parseRetryPolicy(tt.config)
			if gotMax != tt.wantMaxAttempts || gotBackoff != tt.wantBackoffSeconds {
				t.Errorf("parseRetryPolicy(%s) = (%d, %d), want (%d, %d)",
					tt.config, gotMax, gotBackoff, tt.wantMaxAttempts, tt.wantBackoffSeconds)
			}
		})
	}
}

func TestRunDelay(t *testing.T) {
	t.Run("completes after the configured duration", func(t *testing.T) {
		start := time.Now()
		err := runDelay(context.Background(), []byte(`{"seconds":0}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if time.Since(start) > 200*time.Millisecond {
			t.Errorf("a 0-second delay took %v, expected near-instant", time.Since(start))
		}
	})

	t.Run("empty config defaults to zero seconds", func(t *testing.T) {
		if err := runDelay(context.Background(), nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("negative seconds is a non-retriable configuration error", func(t *testing.T) {
		err := runDelay(context.Background(), []byte(`{"seconds":-1}`))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !isNonRetriable(err) {
			t.Error("expected errInvalidDelaySeconds to be non-retriable")
		}
	})

	t.Run("returns ctx.Err when the context is cancelled mid-delay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := runDelay(ctx, []byte(`{"seconds":5}`))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !isNonRetriable(err) {
			t.Error("expected context.Canceled to be classified non-retriable")
		}
	})
}
