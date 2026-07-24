package executor

import (
	"testing"

	"github.com/SahidAyala/Nocturn-Atlas-Workflow-Engine/internal/app/ports"
)

func TestInterpolateStepConfig(t *testing.T) {
	t.Run("empty config is returned unchanged", func(t *testing.T) {
		got, err := interpolateStepConfig(nil, nil, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("got %q, want nil", got)
		}
	})

	t.Run("config with no placeholders passes through untouched", func(t *testing.T) {
		in := []byte(`{"url":"https://example.com"}`)
		got, err := interpolateStepConfig(in, nil, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != string(in) {
			t.Fatalf("got %s, want %s", got, in)
		}
	})

	t.Run("substitutes a scalar field from a prior step's output", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			{StepIndex: 0, Output: []byte(`{"id": 42, "name": "acme"}`)},
		}
		in := []byte(`{"url":"https://example.com/users/{{steps.0.output.id}}"}`)
		got, err := interpolateStepConfig(in, rows, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `{"url":"https://example.com/users/42"}`
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("substitutes a nested dotted path", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			{StepIndex: 0, Output: []byte(`{"body": {"user": {"email": "a@b.com"}}}`)},
		}
		in := []byte(`{"to":"{{steps.0.output.body.user.email}}"}`)
		got, err := interpolateStepConfig(in, rows, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `{"to":"a@b.com"}`
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("substitutes an array index in the path", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			{StepIndex: 0, Output: []byte(`{"items": ["first", "second"]}`)},
		}
		in := []byte(`{"v":"{{steps.0.output.items.1}}"}`)
		got, err := interpolateStepConfig(in, rows, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `{"v":"second"}`
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("a placeholder referencing a step at or after the current index is ignored (left blank), never a forward read", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			// Step 2's own output exists in `rows` (e.g. a retry re-read), but since
			// currentStepIndex is 2, it must NOT be resolved — only step_index < current.
			{StepIndex: 2, Output: []byte(`{"id": 99}`)},
		}
		in := []byte(`{"v":"{{steps.2.output.id}}"}`)
		got, err := interpolateStepConfig(in, rows, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `{"v":""}`
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("missing step output defaults to an empty object, so any path lookup is blank", func(t *testing.T) {
		in := []byte(`{"v":"{{steps.0.output.id}}"}`)
		got, err := interpolateStepConfig(in, nil, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `{"v":""}`
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("a path that does not exist on the output resolves to blank, not an error", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			{StepIndex: 0, Output: []byte(`{"id": 1}`)},
		}
		in := []byte(`{"v":"{{steps.0.output.does.not.exist}}"}`)
		got, err := interpolateStepConfig(in, rows, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `{"v":""}`
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("multiple placeholders across different steps are all substituted", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			{StepIndex: 0, Output: []byte(`{"id": 1}`)},
			{StepIndex: 1, Output: []byte(`{"token": "abc"}`)},
		}
		in := []byte(`{"id":"{{steps.0.output.id}}","auth":"Bearer {{steps.1.output.token}}"}`)
		got, err := interpolateStepConfig(in, rows, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `{"id":"1","auth":"Bearer abc"}`
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("a string value is spliced without surrounding quotes (already inside a JSON string)", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			{StepIndex: 0, Output: []byte(`{"name": "has \"quotes\" inside"}`)},
		}
		in := []byte(`{"v":"{{steps.0.output.name}}"}`)
		got, err := interpolateStepConfig(in, rows, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// The escaped quotes must remain escaped so the overall document is valid JSON.
		want := `{"v":"has \"quotes\" inside"}`
		if string(got) != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("returns an error when a referenced step's stored output is not valid JSON", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			{StepIndex: 0, Output: []byte(`not json`)},
		}
		in := []byte(`{"v":"{{steps.0.output.id}}"}`)
		_, err := interpolateStepConfig(in, rows, 1)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("returns an error when the interpolated result is not valid JSON", func(t *testing.T) {
		rows := []ports.StepIndexOutput{
			{StepIndex: 0, Output: []byte(`{"v": "a\"b"}`)},
		}
		// Deliberately malformed template (unescaped quote breaks the surrounding JSON).
		in := []byte(`{"v": "{{steps.0.output.v}}}`)
		_, err := interpolateStepConfig(in, rows, 1)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}

func TestJSONLookup(t *testing.T) {
	root := map[string]any{
		"a": map[string]any{
			"b": []any{"x", "y", map[string]any{"c": 7.0}},
		},
	}

	t.Run("resolves a nested path through objects and arrays", func(t *testing.T) {
		got, ok := jsonLookup(root, []string{"a", "b", "2", "c"})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if got != 7.0 {
			t.Fatalf("got %v, want 7.0", got)
		}
	})

	t.Run("missing key returns ok=false", func(t *testing.T) {
		_, ok := jsonLookup(root, []string{"a", "nope"})
		if ok {
			t.Fatal("expected ok=false")
		}
	})

	t.Run("out-of-range array index returns ok=false", func(t *testing.T) {
		_, ok := jsonLookup(root, []string{"a", "b", "99"})
		if ok {
			t.Fatal("expected ok=false")
		}
	})

	t.Run("non-numeric index into an array returns ok=false", func(t *testing.T) {
		_, ok := jsonLookup(root, []string{"a", "b", "notanumber"})
		if ok {
			t.Fatal("expected ok=false")
		}
	})

	t.Run("indexing into a scalar returns ok=false", func(t *testing.T) {
		_, ok := jsonLookup(root, []string{"a", "b", "0", "further"})
		if ok {
			t.Fatal("expected ok=false")
		}
	})

	t.Run("empty path returns the root itself", func(t *testing.T) {
		got, ok := jsonLookup(root, nil)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if _, isMap := got.(map[string]any); !isMap {
			t.Fatalf("got %T, want map[string]any", got)
		}
	})
}
