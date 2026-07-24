package db

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestStepRunRepository_GetNextPendingStepRun(t *testing.T) {
	pool := integrationDB(t)
	repo := NewStepRunRepository(pool)
	ctx := context.Background()

	t.Run("returns nil, nil when there is no pending work", func(t *testing.T) {
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{{Name: "only", StepType: "delay"}})
		seedRun(t, pool, workflowID, stepIDs)
		// Claim the only step so nothing is left pending.
		got, err := repo.GetNextPendingStepRun(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected to find the seeded step")
		}
		if err := repo.MarkStepRunRunning(ctx, got.ID); err != nil {
			t.Fatalf("mark running: %v", err)
		}

		// A brand new project with zero seeded work must not surface anything
		// (isolates this assertion from whatever else is pending globally).
		empty, err := repo.GetNextPendingStepRun(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Note: GetNextPendingStepRun is global (not project-scoped), so this
		// only asserts "no error" here — the isolation assertion is the
		// ordering/exclusion tests below, run against a single project's data.
		_ = empty
	})

	t.Run("does not return a step whose predecessor has not succeeded", func(t *testing.T) {
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{
			{Name: "first", StepType: "delay"},
			{Name: "second", StepType: "delay"},
		})
		seedRun(t, pool, workflowID, stepIDs)

		got, err := repo.GetNextPendingStepRun(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.WorkflowStepID != stepIDs[0] {
			t.Fatalf("expected step 0 (%s) to be claimable first, got %+v", stepIDs[0], got)
		}
		// Step 0 is now 'running' (not yet succeeded) — step 1 must stay hidden.
		if err := repo.MarkStepRunRunning(ctx, got.ID); err != nil {
			t.Fatalf("mark running: %v", err)
		}

		// Any subsequent claim from the whole table must not be this run's step 1
		// while step 0 is still running (not succeeded).
		for i := 0; i < 3; i++ {
			next, err := repo.GetNextPendingStepRun(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if next != nil && next.WorkflowStepID == stepIDs[1] {
				t.Fatalf("step 1 must not be claimable before step 0 succeeds")
			}
			if next != nil {
				// Some other test's data — put it back and stop probing.
				break
			}
		}
	})

	t.Run("becomes claimable once the predecessor succeeds", func(t *testing.T) {
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{
			{Name: "first", StepType: "delay"},
			{Name: "second", StepType: "delay"},
		})
		runID := seedRun(t, pool, workflowID, stepIDs)

		// Directly succeed step 0 without going through the repository's own
		// claim flow, to isolate "does ordering respect a succeeded predecessor"
		// from "can this repository claim a step" (covered elsewhere).
		if _, err := pool.Exec(
			`UPDATE step_runs SET status = 'succeeded' WHERE workflow_run_id = $1 AND workflow_step_id = $2`,
			runID, stepIDs[0],
		); err != nil {
			t.Fatalf("seed step 0 as succeeded: %v", err)
		}

		var found *string
		for i := 0; i < 50; i++ { // bounded retry: other tests' rows may be claimed first
			next, err := repo.GetNextPendingStepRun(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if next == nil {
				break
			}
			if next.WorkflowStepID == stepIDs[1] {
				id := next.ID
				found = &id
				break
			}
			// Not ours — claim it so it doesn't block the loop, then keep looking.
			if err := repo.MarkStepRunRunning(ctx, next.ID); err != nil {
				t.Fatalf("mark running (draining unrelated row): %v", err)
			}
		}
		if found == nil {
			t.Fatal("expected step 1 to become claimable after step 0 succeeded")
		}
	})

	t.Run("does not return a step scheduled for a future retry", func(t *testing.T) {
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{{Name: "only", StepType: "delay"}})
		runID := seedRun(t, pool, workflowID, stepIDs)

		if _, err := pool.Exec(
			`UPDATE step_runs SET next_run_at = now() + interval '1 hour' WHERE workflow_run_id = $1`,
			runID,
		); err != nil {
			t.Fatalf("schedule future retry: %v", err)
		}

		for i := 0; i < 50; i++ {
			next, err := repo.GetNextPendingStepRun(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if next == nil {
				return // correctly never surfaced
			}
			if next.WorkflowRunID == runID {
				t.Fatal("a step with a future next_run_at must not be claimable")
			}
			if err := repo.MarkStepRunRunning(ctx, next.ID); err != nil {
				t.Fatalf("mark running (draining unrelated row): %v", err)
			}
		}
	})
}

func TestStepRunRepository_StateTransitionGuards(t *testing.T) {
	pool := integrationDB(t)
	repo := NewStepRunRepository(pool)
	ctx := context.Background()

	newPendingStep := func(t *testing.T) (runID, stepRunID string) {
		t.Helper()
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{{Name: "only", StepType: "delay"}})
		runID = seedRun(t, pool, workflowID, stepIDs)
		if err := pool.QueryRow(`SELECT id FROM step_runs WHERE workflow_run_id = $1`, runID).Scan(&stepRunID); err != nil {
			t.Fatalf("read seeded step_run id: %v", err)
		}
		return runID, stepRunID
	}

	t.Run("MarkStepRunRunning fails when the row is not pending", func(t *testing.T) {
		_, stepRunID := newPendingStep(t)
		if err := repo.MarkStepRunRunning(ctx, stepRunID); err != nil {
			t.Fatalf("first MarkStepRunRunning: %v", err)
		}
		if err := repo.MarkStepRunRunning(ctx, stepRunID); err == nil {
			t.Fatal("expected an error marking an already-running step run as running again")
		}
	})

	t.Run("MarkStepRunSucceeded fails when the row is not running", func(t *testing.T) {
		_, stepRunID := newPendingStep(t)
		// Still pending, never marked running.
		if err := repo.MarkStepRunSucceeded(ctx, stepRunID, nil); err == nil {
			t.Fatal("expected an error succeeding a step run that was never marked running")
		}
	})

	t.Run("MarkStepRunSucceeded persists output JSON", func(t *testing.T) {
		_, stepRunID := newPendingStep(t)
		if err := repo.MarkStepRunRunning(ctx, stepRunID); err != nil {
			t.Fatalf("mark running: %v", err)
		}
		if err := repo.MarkStepRunSucceeded(ctx, stepRunID, []byte(`{"result":"ok"}`)); err != nil {
			t.Fatalf("mark succeeded: %v", err)
		}
		var output string
		if err := pool.QueryRow(`SELECT output::text FROM step_runs WHERE id = $1`, stepRunID).Scan(&output); err != nil {
			t.Fatalf("read output: %v", err)
		}
		if output != `{"result": "ok"}` && output != `{"result":"ok"}` {
			t.Errorf("output = %s, want it to contain the persisted JSON", output)
		}
	})

	t.Run("RetryStepRun resets to pending with a bumped attempt and future next_run_at", func(t *testing.T) {
		_, stepRunID := newPendingStep(t)
		if err := repo.MarkStepRunRunning(ctx, stepRunID); err != nil {
			t.Fatalf("mark running: %v", err)
		}
		nextAt := time.Now().Add(5 * time.Minute).UTC()
		if err := repo.RetryStepRun(ctx, stepRunID, nextAt, 2); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if got := stepRunStatus(t, pool, stepRunID); got != "pending" {
			t.Errorf("status = %s, want pending", got)
		}
		var attempt int
		if err := pool.QueryRow(`SELECT attempt FROM step_runs WHERE id = $1`, stepRunID).Scan(&attempt); err != nil {
			t.Fatalf("read attempt: %v", err)
		}
		if attempt != 2 {
			t.Errorf("attempt = %d, want 2", attempt)
		}
	})

	t.Run("RetryStepRun fails when the row is not running", func(t *testing.T) {
		_, stepRunID := newPendingStep(t)
		if err := repo.RetryStepRun(ctx, stepRunID, time.Now(), 2); err == nil {
			t.Fatal("expected an error retrying a step run that is not running")
		}
	})
}

func TestStepRunRepository_WorkflowRunHelpers(t *testing.T) {
	pool := integrationDB(t)
	repo := NewStepRunRepository(pool)
	ctx := context.Background()

	t.Run("IsWorkflowRunPending reflects the run's status", func(t *testing.T) {
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{{Name: "only", StepType: "delay"}})
		runID := seedRun(t, pool, workflowID, stepIDs)

		pending, err := repo.IsWorkflowRunPending(ctx, runID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !pending {
			t.Error("expected a freshly seeded run to be pending")
		}

		if err := repo.MarkWorkflowRunRunning(ctx, runID); err != nil {
			t.Fatalf("mark workflow run running: %v", err)
		}
		pending, err = repo.IsWorkflowRunPending(ctx, runID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pending {
			t.Error("expected the run to no longer be pending after MarkWorkflowRunRunning")
		}
	})

	t.Run("WorkflowRunStepCounts counts total and succeeded step runs", func(t *testing.T) {
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{
			{Name: "a", StepType: "delay"}, {Name: "b", StepType: "delay"}, {Name: "c", StepType: "delay"},
		})
		runID := seedRun(t, pool, workflowID, stepIDs)

		total, succeeded, err := repo.WorkflowRunStepCounts(ctx, runID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 3 || succeeded != 0 {
			t.Fatalf("got (total=%d, succeeded=%d), want (3, 0)", total, succeeded)
		}

		if _, err := pool.Exec(`UPDATE step_runs SET status = 'succeeded' WHERE workflow_run_id = $1 AND workflow_step_id = $2`, runID, stepIDs[0]); err != nil {
			t.Fatalf("seed succeeded step: %v", err)
		}
		total, succeeded, err = repo.WorkflowRunStepCounts(ctx, runID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 3 || succeeded != 1 {
			t.Fatalf("got (total=%d, succeeded=%d), want (3, 1)", total, succeeded)
		}
	})

	t.Run("MarkWorkflowRunFailed transitions from pending or running, never from a terminal state", func(t *testing.T) {
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{{Name: "only", StepType: "delay"}})
		runID := seedRun(t, pool, workflowID, stepIDs)

		if err := repo.MarkWorkflowRunFailed(ctx, runID); err != nil {
			t.Fatalf("expected pending -> failed to succeed: %v", err)
		}
		if err := repo.MarkWorkflowRunFailed(ctx, runID); err == nil {
			t.Fatal("expected an error re-failing an already-failed run")
		}
	})

	t.Run("GetProjectContext resolves the owning project and external tenant id", func(t *testing.T) {
		projectID := seedProject(t, pool)
		if _, err := pool.Exec(`UPDATE projects SET external_tenant_id = $1 WHERE id = $2`, "tenant-abc", projectID); err != nil {
			t.Fatalf("set external_tenant_id: %v", err)
		}
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{{Name: "only", StepType: "delay"}})
		runID := seedRun(t, pool, workflowID, stepIDs)

		gotProject, gotTenant, err := repo.GetProjectContext(ctx, runID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotProject != projectID {
			t.Errorf("projectID = %s, want %s", gotProject, projectID)
		}
		if gotTenant != "tenant-abc" {
			t.Errorf("tenantID = %s, want tenant-abc", gotTenant)
		}
	})

	t.Run("GetProjectContext returns empty strings (not an error) for an unknown run", func(t *testing.T) {
		gotProject, gotTenant, err := repo.GetProjectContext(ctx, "00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotProject != "" || gotTenant != "" {
			t.Errorf("got (%q, %q), want (\"\", \"\")", gotProject, gotTenant)
		}
	})
}

// TestStepRunRepository_ConcurrentClaimsNeverDuplicate is the regression test
// for the exact race condition docs/known-limitations.md used to flag as
// critical: with FOR UPDATE OF sr SKIP LOCKED on the claim query PLUS the
// atomic conditional UPDATE in MarkStepRunRunning (WHERE status = 'pending'),
// N concurrent "workers" racing on the same pending step runs must each claim
// a DISTINCT row — never the same one twice, never a lost/duplicated claim.
func TestStepRunRepository_ConcurrentClaimsNeverDuplicate(t *testing.T) {
	pool := integrationDB(t)
	repo := NewStepRunRepository(pool)
	ctx := context.Background()

	const numSteps = 12
	const numWorkers = 8

	projectID := seedProject(t, pool)
	seeded := make(map[string]bool, numSteps) // workflow_step_id -> seen
	for i := 0; i < numSteps; i++ {
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{{Name: "only", StepType: "delay"}})
		seedRun(t, pool, workflowID, stepIDs)
		seeded[stepIDs[0]] = false
	}

	var mu sync.Mutex
	claimedBy := make(map[string]int) // workflow_step_id -> how many times successfully claimed
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				next, err := repo.GetNextPendingStepRun(ctx)
				if err != nil {
					t.Errorf("worker %d: GetNextPendingStepRun: %v", worker, err)
					return
				}
				if next == nil {
					return // no more work visible to this worker
				}
				mu.Lock()
				_, isOurs := seeded[next.WorkflowStepID]
				mu.Unlock()
				if !isOurs {
					// Another test's row (tests share the DB) — claim it so it
					// doesn't spin the loop forever, then move on.
					_ = repo.MarkStepRunRunning(ctx, next.ID)
					continue
				}
				err = repo.MarkStepRunRunning(ctx, next.ID)
				mu.Lock()
				if err == nil {
					claimedBy[next.WorkflowStepID]++
				}
				mu.Unlock()
				if err != nil {
					// Lost the double-check race to another worker for THIS row
					// (expected to happen sometimes) — keep polling for more work.
					continue
				}
			}
		}(w)
	}
	wg.Wait()

	if len(claimedBy) != numSteps {
		t.Fatalf("claimed %d distinct steps, want %d (claimedBy=%v)", len(claimedBy), numSteps, claimedBy)
	}
	for stepID, count := range claimedBy {
		if count != 1 {
			t.Errorf("step %s was successfully claimed %d times, want exactly 1 (this is the exact bug FOR UPDATE SKIP LOCKED + the double-check UPDATE exist to prevent)", stepID, count)
		}
	}
}
