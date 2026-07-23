package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/SahidAyala/Nocturn-Atlas-Workflow-Engine/internal/infrastructure/db/models"
)

// TestMapWorkflowToResponse_SnakeCaseJSON guards against the regression this
// type was added to fix: GET /workflows used to serialize models.Workflow
// directly, which has only `db:` tags and defaults to PascalCase JSON,
// while the UI's WorkflowDefinitionRaw (ui/src/lib/api/workflows.ts) expects
// snake_case. See workflowResponse's doc comment.
func TestMapWorkflowToResponse_SnakeCaseJSON(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := created.Add(time.Hour)
	desc := "does a thing"

	wf := models.Workflow{
		ID:          "wf-1",
		ProjectID:   "proj-1",
		Name:        "My Workflow",
		Slug:        "my-workflow",
		Description: &desc,
		CreatedAt:   created,
		UpdatedAt:   updated,
	}

	got := mapWorkflowToResponse(wf)

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantKeys := map[string]string{
		"id":          "wf-1",
		"project_id":  "proj-1",
		"name":        "My Workflow",
		"slug":        "my-workflow",
		"description": "does a thing",
		"created_at":  created.Format(time.RFC3339),
		"updated_at":  updated.Format(time.RFC3339),
	}
	for key, want := range wantKeys {
		got, ok := raw[key]
		if !ok {
			t.Errorf("missing expected snake_case key %q in JSON output: %s", key, body)
			continue
		}
		if got != want {
			t.Errorf("key %q = %v, want %v", key, got, want)
		}
	}

	// The old PascalCase keys must NOT be present — that was the bug.
	for _, badKey := range []string{"ID", "ProjectID", "Name", "Slug", "CreatedAt", "UpdatedAt"} {
		if _, ok := raw[badKey]; ok {
			t.Errorf("PascalCase key %q leaked into JSON output: %s", badKey, body)
		}
	}
}

// TestMapWorkflowToResponse_NilDescriptionOmitted checks the omitempty tag
// keeps a nil description out of the payload rather than emitting `null`.
func TestMapWorkflowToResponse_NilDescriptionOmitted(t *testing.T) {
	wf := models.Workflow{
		ID:        "wf-2",
		ProjectID: "proj-1",
		Name:      "No Description",
		Slug:      "no-description",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	got := mapWorkflowToResponse(wf)
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["description"]; ok {
		t.Errorf("expected description to be omitted when nil, got: %s", body)
	}
}
