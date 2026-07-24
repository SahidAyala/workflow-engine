package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
)

// integrationDB returns a pool against a real PostgreSQL instance, or skips
// the test if DATABASE_URL is not set. Mirrors the pattern used by
// core/libs/audit and events/ for the same reason: the default `go test
// ./...` run must never require a database, but CI (which sets DATABASE_URL
// and provisions a postgres service) exercises these tests for real.
//
// Run locally with, e.g.:
//
//	export DATABASE_URL='postgresql://youruser@localhost:5432/workflow-engine?sslmode=disable'
//	go test ./internal/infrastructure/db/...
func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	pool, err := OpenSQL(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func randSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// seedProject inserts a project row and registers cleanup. Deletes
// workflow_runs before the project (which cascades to workflows/steps) since
// workflow_runs -> workflows is ON DELETE RESTRICT, not CASCADE — a bare
// `DELETE FROM projects` would fail with a foreign key violation if any test
// left a run behind.
func seedProject(t *testing.T, db *sql.DB) string {
	t.Helper()
	suffix := randSuffix(t)
	var id string
	err := db.QueryRow(
		`INSERT INTO projects (name, slug) VALUES ($1, $2) RETURNING id`,
		"test-project-"+suffix, "test-project-"+suffix,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_runs WHERE workflow_id IN (SELECT id FROM workflows WHERE project_id = $1)`, id)
		_, _ = db.Exec(`DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

// seedWorkflowStep describes one step to seed via seedWorkflow.
type seedWorkflowStep struct {
	Name     string
	StepType string
	Config   string // raw JSON; "" becomes "{}"
}

// seedWorkflow inserts a workflow with the given steps (in order, step_index
// starting at 0) under projectID. Returns the workflow id and the step ids in
// step_index order. Cleanup is handled by the owning project's seedProject.
func seedWorkflow(t *testing.T, db *sql.DB, projectID string, steps []seedWorkflowStep) (workflowID string, stepIDs []string) {
	t.Helper()
	suffix := randSuffix(t)
	err := db.QueryRow(
		`INSERT INTO workflows (project_id, name, slug) VALUES ($1, $2, $3) RETURNING id`,
		projectID, "test-workflow-"+suffix, "test-workflow-"+suffix,
	).Scan(&workflowID)
	if err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	for i, st := range steps {
		cfg := st.Config
		if cfg == "" {
			cfg = "{}"
		}
		var stepID string
		err := db.QueryRow(
			`INSERT INTO workflow_steps (workflow_id, step_index, name, step_type, config)
			 VALUES ($1, $2, $3, $4, $5::jsonb) RETURNING id`,
			workflowID, i, st.Name, st.StepType, cfg,
		).Scan(&stepID)
		if err != nil {
			t.Fatalf("seed workflow step %d: %v", i, err)
		}
		stepIDs = append(stepIDs, stepID)
	}
	return workflowID, stepIDs
}

// seedRun inserts a workflow_run and one pending step_run (attempt 1) per
// stepID, in the given order. Returns the run id.
func seedRun(t *testing.T, db *sql.DB, workflowID string, stepIDs []string) string {
	t.Helper()
	var runID string
	err := db.QueryRow(
		`INSERT INTO workflow_runs (workflow_id, status, input) VALUES ($1, 'pending', '{}'::jsonb) RETURNING id`,
		workflowID,
	).Scan(&runID)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for _, stepID := range stepIDs {
		if _, err := db.Exec(
			`INSERT INTO step_runs (workflow_run_id, workflow_step_id, attempt, status, input)
			 VALUES ($1, $2, 1, 'pending', '{}'::jsonb)`,
			runID, stepID,
		); err != nil {
			t.Fatalf("seed step_run: %v", err)
		}
	}
	return runID
}

// stepRunStatus reads back a single step_run's status column, for assertions.
func stepRunStatus(t *testing.T, db *sql.DB, stepRunID string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM step_runs WHERE id = $1`, stepRunID).Scan(&status); err != nil {
		t.Fatalf("read step_run status: %v", err)
	}
	return status
}
