package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/SahidAyala/Nocturn-Atlas-Workflow-Engine/internal/app"
	"github.com/SahidAyala/Nocturn-Atlas-Workflow-Engine/internal/auth"
	"github.com/SahidAyala/Nocturn-Atlas-Workflow-Engine/internal/infrastructure/db"
	"github.com/SahidAyala/Nocturn-Atlas-Workflow-Engine/internal/infrastructure/db/models"
	"github.com/SahidAyala/Nocturn-Atlas-Workflow-Engine/internal/interfaces/http/respond"
)

const maxCreateWorkflowBody = 1 << 20 // 1 MiB

// WorkflowHandler serves HTTP for workflows (definitions under a project).
type WorkflowHandler struct {
	svc *app.WorkflowService
}

// NewWorkflowHandler wires the handler with its dependencies.
func NewWorkflowHandler(svc *app.WorkflowService) *WorkflowHandler {
	return &WorkflowHandler{svc: svc}
}

// CreateWorkflowRequest is the JSON body for POST /workflows.
type CreateWorkflowRequest struct {
	Name  string                      `json:"name"`
	Steps []CreateWorkflowStepRequest `json:"steps"`
}

// CreateWorkflowStepRequest is one step in the create payload.
type CreateWorkflowStepRequest struct {
	Name string `json:"name" example:"Fetch user"`
	// Type is the executor step kind: delay, http_request.
	Type string `json:"type" enums:"delay http_request" example:"http_request"`
	// Config is step-specific JSON. delay: {"seconds":1}. http_request: {"method":"GET","url":"https://example.com","headers":{},"body":{}}. Optional retry: {"max_attempts":3,"backoff_seconds":5}. Strings may include {{steps.0.output.body.id}}.
	Config json.RawMessage `json:"config" swaggertype:"object"`
}

// CreateWorkflowResponse is returned after creating a workflow definition.
type CreateWorkflowResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	StepsCount int    `json:"steps_count"`
}

// CreateWorkflowRunResponse is returned after POST /workflows/{id}/runs.
type CreateWorkflowRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Steps  int    `json:"steps"`
}

// CreateWorkflow handles POST /workflows (authenticated).
//
//	@Summary		Create workflow
//	@Description	Creates a workflow with ordered steps (`steps[0]`, `steps[1]`, …) for the authenticated project. Each step has `type` and `config` JSON; see `CreateWorkflowStepRequest`. Steps run in order in the worker; later steps can reference earlier outputs via `{{steps.N.output...}}` in config strings.
//	@Tags			workflows
//	@Accept			json
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			body	body		CreateWorkflowRequest	true	"Workflow definition"
//	@Success		201		{object}	CreateWorkflowResponse
//	@Failure		400		{object}	JSONError
//	@Failure		401		{object}	JSONError
//	@Failure		500		{object}	JSONError
//	@Router			/workflows [post]
func (h *WorkflowHandler) CreateWorkflow(w http.ResponseWriter, r *http.Request) {
	projectID, ok := auth.ProjectIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCreateWorkflowBody)
	var req CreateWorkflowRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid json")
		return
	}

	steps := make([]app.CreateWorkflowStepInput, 0, len(req.Steps))
	for _, s := range req.Steps {
		cfg := s.Config
		if len(cfg) == 0 {
			cfg = json.RawMessage(`{}`)
		}
		steps = append(steps, app.CreateWorkflowStepInput{
			Name:     s.Name,
			StepType: s.Type,
			Config:   cfg,
		})
	}

	out, err := h.svc.CreateWorkflow(r.Context(), projectID, app.CreateWorkflowInput{
		Name:  req.Name,
		Steps: steps,
	})
	if err != nil {
		if msg, ok := app.AsValidation(err); ok {
			respond.Error(w, http.StatusBadRequest, msg)
			return
		}
		log.Printf("workflows: create: %v", err)
		respond.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	respond.JSON(w, http.StatusCreated, CreateWorkflowResponse{
		ID:         out.ID,
		Name:       out.Name,
		StepsCount: out.StepsCount,
	})
}

// CreateWorkflowRun handles POST /workflows/{id}/runs (authenticated).
//
//	@Summary		Start workflow run
//	@Description	Creates a `workflow_run` in status pending and one `step_run` per definition step (pending, attempt 1). The worker process picks up and executes steps; this endpoint only enqueues work.
//	@Tags			workflows
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			id	path		string	true	"Workflow ID"
//	@Success		201	{object}	CreateWorkflowRunResponse
//	@Failure		400	{object}	JSONError
//	@Failure		401	{object}	JSONError
//	@Failure		404	{object}	JSONError
//	@Failure		500	{object}	JSONError
//	@Router			/workflows/{id}/runs [post]
func (h *WorkflowHandler) CreateWorkflowRun(w http.ResponseWriter, r *http.Request) {
	projectID, ok := auth.ProjectIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	wfID := strings.TrimSpace(r.PathValue("id"))
	if wfID == "" {
		respond.Error(w, http.StatusBadRequest, "workflow id is required")
		return
	}

	out, err := h.svc.StartWorkflowRun(r.Context(), projectID, wfID)
	if err != nil {
		if msg, ok := app.AsValidation(err); ok {
			respond.Error(w, http.StatusBadRequest, msg)
			return
		}
		if errors.Is(err, db.ErrWorkflowNotFound) {
			respond.Error(w, http.StatusNotFound, "workflow not found")
			return
		}
		log.Printf("workflows: create run: %v", err)
		respond.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	respond.JSON(w, http.StatusCreated, CreateWorkflowRunResponse{
		RunID:  out.RunID,
		Status: out.Status,
		Steps:  out.Steps,
	})
}

// GetAllWorkflows handles GET /workflows: list workflows for the authenticated project.
//
//	@Summary		List workflows
//	@Description	Returns workflow definitions for the project from the API key.
//	@Tags			workflows
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Success		200	{array}	workflowResponse
//	@Failure		401	{object}	JSONError
//	@Failure		500	{object}	JSONError
//	@Router			/workflows [get]
func (h *WorkflowHandler) GetAllWorkflows(w http.ResponseWriter, r *http.Request) {
	projectID, ok := auth.ProjectIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	workflows, err := h.svc.ListByProject(r.Context(), projectID)
	if err != nil {
		log.Printf("workflows: get all: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	resp := make([]workflowResponse, 0, len(workflows))
	for _, wf := range workflows {
		resp = append(resp, mapWorkflowToResponse(wf))
	}
	writeJSON(w, http.StatusOK, resp)
}

// workflowResponse is the JSON shape for a workflow definition. Snake_case to
// match the UI's WorkflowDefinitionRaw / mapDefinition() contract
// (ui/src/lib/api/workflows.ts) — models.Workflow carries only `db:` tags and
// must never be serialized directly (it defaults to PascalCase and was the
// root cause of the workflows list rendering empty in the UI).
type workflowResponse struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"project_id"`
	Name        string  `json:"name"`
	Slug        string  `json:"slug"`
	Description *string `json:"description,omitempty"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func mapWorkflowToResponse(wf models.Workflow) workflowResponse {
	return workflowResponse{
		ID:          wf.ID,
		ProjectID:   wf.ProjectID,
		Name:        wf.Name,
		Slug:        wf.Slug,
		Description: wf.Description,
		CreatedAt:   wf.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   wf.UpdatedAt.Format(time.RFC3339),
	}
}

// workflowRunResponse is the JSON shape for a single workflow run (matches UI WorkflowRun type).
type workflowRunResponse struct {
	ID            string            `json:"id"`
	WorkflowID    string            `json:"workflowId"`
	WorkflowName  string            `json:"workflowName"`
	Status        string            `json:"status"`
	Input         json.RawMessage   `json:"input,omitempty"`
	Output        *json.RawMessage  `json:"output,omitempty"`
	ErrorMessage  *string           `json:"errorMessage,omitempty"`
	CorrelationID *string           `json:"correlationId,omitempty"`
	CreatedAt     string            `json:"createdAt"`
	StartedAt     *string           `json:"startedAt,omitempty"`
	CompletedAt   *string           `json:"completedAt,omitempty"`
	DurationMs    *int64            `json:"durationMs,omitempty"`
	StepRuns      []stepRunResponse `json:"stepRuns"`
}

// stepRunResponse is the JSON shape for a step run (matches UI StepRun type).
type stepRunResponse struct {
	ID             string           `json:"id"`
	WorkflowRunID  string           `json:"workflowRunId"`
	WorkflowStepID string           `json:"workflowStepId"`
	StepName       string           `json:"stepName"`
	StepType       string           `json:"stepType"`
	StepIndex      int              `json:"stepIndex"`
	Attempt        int              `json:"attempt"`
	Status         string           `json:"status"`
	Input          json.RawMessage  `json:"input,omitempty"`
	Output         *json.RawMessage `json:"output,omitempty"`
	ErrorMessage   *string          `json:"errorMessage,omitempty"`
	CreatedAt      string           `json:"createdAt"`
	StartedAt      *string          `json:"startedAt,omitempty"`
	CompletedAt    *string          `json:"completedAt,omitempty"`
	DurationMs     *int64           `json:"durationMs,omitempty"`
}

// listRunsResponse matches the UI ApiListResponse<WorkflowRun> shape.
type listRunsResponse struct {
	Items []workflowRunResponse `json:"items"`
	Total int                   `json:"total"`
	Page  int                   `json:"page"`
	Limit int                   `json:"limit"`
}

func mapRunToResponse(item db.RunListItem) workflowRunResponse {
	r := workflowRunResponse{
		ID:            item.ID,
		WorkflowID:    item.WorkflowID,
		WorkflowName:  item.WorkflowName,
		Status:        item.Status,
		Input:         item.Input,
		Output:        item.Output,
		ErrorMessage:  item.ErrorMessage,
		CorrelationID: item.CorrelationID,
		CreatedAt:     item.CreatedAt.Format(time.RFC3339),
		StepRuns:      []stepRunResponse{},
	}
	if item.StartedAt != nil {
		s := item.StartedAt.Format(time.RFC3339)
		r.StartedAt = &s
	}
	if item.CompletedAt != nil {
		s := item.CompletedAt.Format(time.RFC3339)
		r.CompletedAt = &s
		if item.StartedAt != nil {
			ms := item.CompletedAt.Sub(*item.StartedAt).Milliseconds()
			r.DurationMs = &ms
		}
	}
	return r
}

func mapStepRunToResponse(sr db.StepRunDetail) stepRunResponse {
	s := stepRunResponse{
		ID:             sr.ID,
		WorkflowRunID:  sr.WorkflowRunID,
		WorkflowStepID: sr.WorkflowStepID,
		StepName:       sr.StepName,
		StepType:       sr.StepType,
		StepIndex:      sr.StepIndex,
		Attempt:        sr.Attempt,
		Status:         sr.Status,
		Input:          sr.Input,
		Output:         sr.Output,
		ErrorMessage:   sr.ErrorMessage,
		CreatedAt:      sr.CreatedAt.Format(time.RFC3339),
	}
	if sr.StartedAt != nil {
		t := sr.StartedAt.Format(time.RFC3339)
		s.StartedAt = &t
	}
	if sr.CompletedAt != nil {
		t := sr.CompletedAt.Format(time.RFC3339)
		s.CompletedAt = &t
		if sr.StartedAt != nil {
			ms := sr.CompletedAt.Sub(*sr.StartedAt).Milliseconds()
			s.DurationMs = &ms
		}
	}
	return s
}

// ListAllRuns handles GET /workflows/runs: list all runs for the authenticated project.
func (h *WorkflowHandler) ListAllRuns(w http.ResponseWriter, r *http.Request) {
	projectID, ok := auth.ProjectIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	items, err := h.svc.ListRunsByProject(r.Context(), projectID)
	if err != nil {
		log.Printf("workflows: list runs: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	resp := make([]workflowRunResponse, 0, len(items))
	for _, item := range items {
		resp = append(resp, mapRunToResponse(item))
	}
	writeJSON(w, http.StatusOK, listRunsResponse{Items: resp, Total: len(resp), Page: 1, Limit: 100})
}

// GetRun handles GET /workflows/runs/{id}: get a single run with step detail.
func (h *WorkflowHandler) GetRun(w http.ResponseWriter, r *http.Request) {
	projectID, ok := auth.ProjectIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	if runID == "" {
		writeJSONError(w, http.StatusBadRequest, "run id is required")
		return
	}
	detail, err := h.svc.GetRunDetail(r.Context(), projectID, runID)
	if err != nil {
		if errors.Is(err, db.ErrRunNotFound) {
			writeJSONError(w, http.StatusNotFound, "run not found")
			return
		}
		log.Printf("workflows: get run: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	resp := mapRunToResponse(detail.RunListItem)
	resp.StepRuns = make([]stepRunResponse, 0, len(detail.StepRuns))
	for _, sr := range detail.StepRuns {
		resp.StepRuns = append(resp.StepRuns, mapStepRunToResponse(sr))
	}
	writeJSON(w, http.StatusOK, resp)
}
