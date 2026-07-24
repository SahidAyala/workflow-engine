package app

import (
	"context"
	"strings"
	"testing"
)

// WorkflowService.repo is a *db.WorkflowRepository (a concrete struct, not an
// interface), so it cannot be faked without a real database connection.
// These tests exercise every validation rule in CreateWorkflow/StartWorkflowRun
// that returns before ever touching s.repo — passing a nil repo and relying on
// that short-circuit is itself part of what's being verified: if any of these
// paths accidentally started dereferencing repo, the test would panic instead
// of returning a ValidationError.
func newServiceWithNilRepo() *WorkflowService {
	return NewWorkflowService(nil)
}

func TestCreateWorkflow_Validation(t *testing.T) {
	svc := newServiceWithNilRepo()
	ctx := context.Background()

	tests := []struct {
		name    string
		in      CreateWorkflowInput
		wantMsg string
	}{
		{
			name:    "empty name",
			in:      CreateWorkflowInput{Name: "  ", Steps: []CreateWorkflowStepInput{{Name: "a", StepType: "delay"}}},
			wantMsg: "name is required",
		},
		{
			name: "name too long",
			in: CreateWorkflowInput{
				Name:  strings.Repeat("a", 201),
				Steps: []CreateWorkflowStepInput{{Name: "a", StepType: "delay"}},
			},
			wantMsg: "name must be at most 200 characters",
		},
		{
			name:    "no steps",
			in:      CreateWorkflowInput{Name: "ok", Steps: nil},
			wantMsg: "steps must not be empty",
		},
		{
			name: "a step with an empty name",
			in: CreateWorkflowInput{
				Name:  "ok",
				Steps: []CreateWorkflowStepInput{{Name: "  ", StepType: "delay"}},
			},
			wantMsg: "each step must have a non-empty name",
		},
		{
			name: "duplicate step names",
			in: CreateWorkflowInput{
				Name: "ok",
				Steps: []CreateWorkflowStepInput{
					{Name: "dup", StepType: "delay"},
					{Name: "dup", StepType: "http_request"},
				},
			},
			wantMsg: "step names must be unique within the workflow",
		},
		{
			name: "a step with an empty type",
			in: CreateWorkflowInput{
				Name:  "ok",
				Steps: []CreateWorkflowStepInput{{Name: "a", StepType: "  "}},
			},
			wantMsg: "each step must have a non-empty type",
		},
		{
			name: "invalid JSON step config",
			in: CreateWorkflowInput{
				Name:  "ok",
				Steps: []CreateWorkflowStepInput{{Name: "a", StepType: "delay", Config: []byte("not json")}},
			},
			wantMsg: "each step config must be valid JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.CreateWorkflow(ctx, "project-1", tt.in)
			if err == nil {
				t.Fatal("expected an error")
			}
			msg, ok := AsValidation(err)
			if !ok {
				t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
			}
			if msg != tt.wantMsg {
				t.Errorf("message = %q, want %q", msg, tt.wantMsg)
			}
		})
	}

	t.Run("an empty step config defaults to {} and is accepted (not a validation error)", func(t *testing.T) {
		// Empty config passes validation and proceeds to s.repo.CreateWithSteps,
		// which panics against a nil repo — proving this specific input does
		// NOT short-circuit before the repository call (every case above does).
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected a nil-pointer panic reaching the repository — validation should have passed")
			}
		}()
		_, _ = svc.CreateWorkflow(ctx, "project-1", CreateWorkflowInput{
			Name:  "ok",
			Steps: []CreateWorkflowStepInput{{Name: "a", StepType: "delay"}}, // Config left nil
		})
	})
}

func TestStartWorkflowRun_Validation(t *testing.T) {
	svc := newServiceWithNilRepo()

	t.Run("empty workflow id is rejected before touching the repository", func(t *testing.T) {
		_, err := svc.StartWorkflowRun(context.Background(), "project-1", "   ")
		msg, ok := AsValidation(err)
		if !ok {
			t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
		}
		if msg != "workflow id is required" {
			t.Errorf("message = %q, want %q", msg, "workflow id is required")
		}
	})
}

func TestAsValidation(t *testing.T) {
	t.Run("unwraps a *ValidationError", func(t *testing.T) {
		msg, ok := AsValidation(&ValidationError{Msg: "boom"})
		if !ok || msg != "boom" {
			t.Errorf("got (%q, %v), want (boom, true)", msg, ok)
		}
	})

	t.Run("returns false for any other error", func(t *testing.T) {
		_, ok := AsValidation(errPlain("something else"))
		if ok {
			t.Error("expected ok=false for a non-ValidationError")
		}
	})

	t.Run("returns false for nil", func(t *testing.T) {
		_, ok := AsValidation(nil)
		if ok {
			t.Error("expected ok=false for nil")
		}
	})
}

type errPlain string

func (e errPlain) Error() string { return string(e) }

func TestSlugFromName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple lowercase", "hello world", "hello-world"},
		{"uppercase is lowercased", "Hello World", "hello-world"},
		{"underscores become dashes", "hello_world_again", "hello-world-again"},
		{"punctuation collapses to a single dash", "hello!!!world", "hello-world"},
		{"leading and trailing dashes are trimmed", "  --hello--  ", "hello"},
		{"repeated dashes collapse to one", "a---b", "a-b"},
		{"only punctuation yields an empty slug", "!!!", ""},
		{
			"truncated to 48 chars without a trailing dash",
			strings.Repeat("a", 50),
			strings.Repeat("a", 48),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slugFromName(tt.in); got != tt.want {
				t.Errorf("slugFromName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewWorkflowSlug(t *testing.T) {
	t.Run("appends a random suffix to the base slug", func(t *testing.T) {
		got, err := newWorkflowSlug("My Workflow")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(got, "my-workflow-") {
			t.Errorf("got %q, want a slug starting with my-workflow-", got)
		}
	})

	t.Run("falls back to the literal 'workflow' when the name has no alphanumeric characters", func(t *testing.T) {
		got, err := newWorkflowSlug("!!!")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(got, "workflow-") {
			t.Errorf("got %q, want a slug starting with workflow-", got)
		}
	})

	t.Run("two calls for the same name produce different slugs", func(t *testing.T) {
		a, err := newWorkflowSlug("Same Name")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		b, err := newWorkflowSlug("Same Name")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a == b {
			t.Errorf("expected two distinct random suffixes, got %q twice", a)
		}
	})
}
