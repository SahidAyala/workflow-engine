package db

import (
	"context"
	"errors"
	"testing"
)

func TestWorkflowRepository_CreateWithSteps(t *testing.T) {
	pool := integrationDB(t)
	repo := NewWorkflowRepository(pool)
	ctx := context.Background()

	t.Run("inserts the workflow and its steps in order", func(t *testing.T) {
		projectID := seedProject(t, pool)
		suffix := randSuffix(t)

		id, err := repo.CreateWithSteps(ctx, projectID, "My Workflow", "my-workflow-"+suffix, []WorkflowStepInsert{
			{Name: "first", StepType: "delay", Config: []byte(`{"seconds":1}`)},
			{Name: "second", StepType: "http_request", Config: []byte(`{"method":"GET","url":"https://example.com"}`)},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id == "" {
			t.Fatal("expected a non-empty workflow id")
		}
		t.Cleanup(func() { _, _ = pool.Exec(`DELETE FROM workflows WHERE id = $1`, id) })

		rows, err := pool.Query(`SELECT step_index, name, step_type FROM workflow_steps WHERE workflow_id = $1 ORDER BY step_index`, id)
		if err != nil {
			t.Fatalf("query steps: %v", err)
		}
		defer rows.Close()

		var got []struct {
			Index int
			Name  string
			Type  string
		}
		for rows.Next() {
			var r struct {
				Index int
				Name  string
				Type  string
			}
			if err := rows.Scan(&r.Index, &r.Name, &r.Type); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		if len(got) != 2 {
			t.Fatalf("got %d steps, want 2", len(got))
		}
		if got[0].Index != 0 || got[0].Name != "first" || got[0].Type != "delay" {
			t.Errorf("step 0 = %+v, want {0 first delay}", got[0])
		}
		if got[1].Index != 1 || got[1].Name != "second" || got[1].Type != "http_request" {
			t.Errorf("step 1 = %+v, want {1 second http_request}", got[1])
		}
	})

	t.Run("rolls back entirely if inserting a step fails (no orphaned workflow row)", func(t *testing.T) {
		projectID := seedProject(t, pool)
		suffix := randSuffix(t)
		slug := "rollback-check-" + suffix

		// Config is bound as $5::jsonb inside the transaction; invalid JSON
		// fails that cast, so the second step's INSERT fails after the
		// workflow row (and the first step) were already written in the same
		// transaction. CreateWithSteps must roll the whole thing back.
		_, err := repo.CreateWithSteps(ctx, projectID, "Rollback Check", slug, []WorkflowStepInsert{
			{Name: "good", StepType: "delay", Config: []byte(`{}`)},
			{Name: "bad", StepType: "delay", Config: []byte(`not valid json`)},
		})
		if err == nil {
			t.Fatal("expected an error from the invalid step config")
		}

		var count int
		if err := pool.QueryRow(`SELECT count(*) FROM workflows WHERE project_id = $1 AND slug = $2`, projectID, slug).Scan(&count); err != nil {
			t.Fatalf("count workflows: %v", err)
		}
		if count != 0 {
			t.Errorf("found %d orphaned workflow row(s) after a failed CreateWithSteps — the transaction did not roll back", count)
		}
	})

	t.Run("a duplicate slug within the same project is rejected", func(t *testing.T) {
		projectID := seedProject(t, pool)
		suffix := randSuffix(t)
		slug := "dup-slug-" + suffix

		id1, err := repo.CreateWithSteps(ctx, projectID, "First", slug, []WorkflowStepInsert{{Name: "a", StepType: "delay", Config: []byte(`{}`)}})
		if err != nil {
			t.Fatalf("first create: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(`DELETE FROM workflows WHERE id = $1`, id1) })

		_, err = repo.CreateWithSteps(ctx, projectID, "Second", slug, []WorkflowStepInsert{{Name: "a", StepType: "delay", Config: []byte(`{}`)}})
		if err == nil {
			t.Fatal("expected an error creating a second workflow with the same project+slug")
		}
		if !IsUniqueViolation(err) {
			t.Errorf("expected IsUniqueViolation(err) to be true for %v", err)
		}
	})
}

func TestWorkflowRepository_GetAllByProjectID(t *testing.T) {
	pool := integrationDB(t)
	repo := NewWorkflowRepository(pool)
	ctx := context.Background()

	t.Run("returns only this project's workflows, newest first", func(t *testing.T) {
		projectA := seedProject(t, pool)
		projectB := seedProject(t, pool)

		_, _ = seedWorkflow(t, pool, projectA, []seedWorkflowStep{{Name: "a1", StepType: "delay"}})
		_, _ = seedWorkflow(t, pool, projectB, []seedWorkflowStep{{Name: "b1", StepType: "delay"}})
		wf2, _ := seedWorkflow(t, pool, projectA, []seedWorkflowStep{{Name: "a2", StepType: "delay"}})

		got, err := repo.GetAllByProjectID(ctx, projectA)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d workflows, want 2 (project isolation failed?)", len(got))
		}
		if got[0].ID != wf2 {
			t.Errorf("got[0].ID = %s, want the most recently created workflow %s", got[0].ID, wf2)
		}
		for _, w := range got {
			if w.ProjectID != projectA {
				t.Errorf("workflow %s has ProjectID %s, want %s", w.ID, w.ProjectID, projectA)
			}
		}
	})

	t.Run("returns an empty slice (not nil) for a project with no workflows", func(t *testing.T) {
		projectID := seedProject(t, pool)
		got, err := repo.GetAllByProjectID(ctx, projectID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Error("expected an empty slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("got %d workflows, want 0", len(got))
		}
	})
}

func TestWorkflowRepository_CreateWorkflowRunWithStepRuns(t *testing.T) {
	pool := integrationDB(t)
	repo := NewWorkflowRepository(pool)
	ctx := context.Background()

	t.Run("creates a pending run with one pending step_run per workflow step", func(t *testing.T) {
		projectID := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{
			{Name: "a", StepType: "delay"}, {Name: "b", StepType: "delay"},
		})

		runID, n, err := repo.CreateWorkflowRunWithStepRuns(ctx, projectID, workflowID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != len(stepIDs) {
			t.Errorf("stepCount = %d, want %d", n, len(stepIDs))
		}

		var status string
		if err := pool.QueryRow(`SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&status); err != nil {
			t.Fatalf("read run status: %v", err)
		}
		if status != "pending" {
			t.Errorf("run status = %s, want pending", status)
		}

		var stepRunCount int
		if err := pool.QueryRow(`SELECT count(*) FROM step_runs WHERE workflow_run_id = $1 AND status = 'pending'`, runID).Scan(&stepRunCount); err != nil {
			t.Fatalf("count step runs: %v", err)
		}
		if stepRunCount != 2 {
			t.Errorf("pending step_run count = %d, want 2", stepRunCount)
		}
	})

	t.Run("rejects a workflow id that belongs to a different project", func(t *testing.T) {
		ownerProject := seedProject(t, pool)
		otherProject := seedProject(t, pool)
		workflowID, _ := seedWorkflow(t, pool, ownerProject, []seedWorkflowStep{{Name: "a", StepType: "delay"}})

		_, _, err := repo.CreateWorkflowRunWithStepRuns(ctx, otherProject, workflowID)
		if !errors.Is(err, ErrWorkflowNotFound) {
			t.Errorf("got err=%v, want ErrWorkflowNotFound (cross-project run creation must be rejected)", err)
		}
	})

	t.Run("rejects a workflow id that does not exist", func(t *testing.T) {
		projectID := seedProject(t, pool)
		_, _, err := repo.CreateWorkflowRunWithStepRuns(ctx, projectID, "00000000-0000-0000-0000-000000000000")
		if !errors.Is(err, ErrWorkflowNotFound) {
			t.Errorf("got err=%v, want ErrWorkflowNotFound", err)
		}
	})
}

func TestWorkflowRepository_ListAndGetRun(t *testing.T) {
	pool := integrationDB(t)
	wfRepo := NewWorkflowRepository(pool)
	ctx := context.Background()

	t.Run("ListRunsByProjectID and GetRunWithSteps agree on the same data, scoped to the project", func(t *testing.T) {
		projectID := seedProject(t, pool)
		otherProject := seedProject(t, pool)
		workflowID, stepIDs := seedWorkflow(t, pool, projectID, []seedWorkflowStep{
			{Name: "step-a", StepType: "delay"}, {Name: "step-b", StepType: "http_request"},
		})
		runID := seedRun(t, pool, workflowID, stepIDs)

		runs, err := wfRepo.ListRunsByProjectID(ctx, projectID)
		if err != nil {
			t.Fatalf("ListRunsByProjectID: %v", err)
		}
		if len(runs) != 1 || runs[0].ID != runID {
			t.Fatalf("got %+v, want exactly one run (%s)", runs, runID)
		}

		// Not visible from a different project.
		otherRuns, err := wfRepo.ListRunsByProjectID(ctx, otherProject)
		if err != nil {
			t.Fatalf("ListRunsByProjectID (other project): %v", err)
		}
		if len(otherRuns) != 0 {
			t.Errorf("got %d runs for an unrelated project, want 0", len(otherRuns))
		}

		detail, err := wfRepo.GetRunWithSteps(ctx, projectID, runID)
		if err != nil {
			t.Fatalf("GetRunWithSteps: %v", err)
		}
		if len(detail.StepRuns) != 2 {
			t.Fatalf("got %d step runs, want 2", len(detail.StepRuns))
		}
		if detail.StepRuns[0].StepIndex != 0 || detail.StepRuns[1].StepIndex != 1 {
			t.Errorf("step runs not ordered by step_index: %+v", detail.StepRuns)
		}

		// Cross-project lookup must fail even though the run id is valid.
		if _, err := wfRepo.GetRunWithSteps(ctx, otherProject, runID); !errors.Is(err, ErrRunNotFound) {
			t.Errorf("got err=%v, want ErrRunNotFound for a cross-project lookup", err)
		}
	})

	t.Run("GetRunWithSteps returns ErrRunNotFound for an unknown run id", func(t *testing.T) {
		projectID := seedProject(t, pool)
		_, err := wfRepo.GetRunWithSteps(ctx, projectID, "00000000-0000-0000-0000-000000000000")
		if !errors.Is(err, ErrRunNotFound) {
			t.Errorf("got err=%v, want ErrRunNotFound", err)
		}
	})
}
