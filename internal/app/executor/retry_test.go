package executor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryDelayWithJitter(t *testing.T) {
	t.Run("doubles per attempt within jitter bounds", func(t *testing.T) {
		cases := []struct {
			failedAttempt int
			base          int
		}{
			{1, 5},
			{2, 5},
			{3, 5},
			{4, 5},
		}
		for _, c := range cases {
			expected := float64(c.base) * pow2(c.failedAttempt-1)
			lo := time.Duration(expected * 0.8 * float64(time.Second))
			hi := time.Duration(expected * 1.2 * float64(time.Second))
			for i := 0; i < 20; i++ {
				got := retryDelayWithJitter(c.base, c.failedAttempt)
				if got < lo || got > hi {
					t.Fatalf("attempt %d: got %v, want within [%v, %v]", c.failedAttempt, got, lo, hi)
				}
			}
		}
	})

	t.Run("caps at maxRetryDelay for a large base/attempt", func(t *testing.T) {
		got := retryDelayWithJitter(3600, 20) // would be enormous uncapped
		// Capped value still gets jitter applied on top, so allow the ±20% band
		// around the cap rather than an exact bound.
		if got > time.Duration(float64(maxRetryDelay)*1.21) {
			t.Fatalf("got %v, want <= ~%v (cap + jitter)", got, maxRetryDelay)
		}
		if got <= 0 {
			t.Fatalf("got non-positive delay %v", got)
		}
	})

	t.Run("negative base is treated as zero", func(t *testing.T) {
		got := retryDelayWithJitter(-5, 1)
		if got != 0 {
			t.Fatalf("got %v, want 0", got)
		}
	})

	t.Run("failedAttempt below 1 is treated as 1", func(t *testing.T) {
		// Both should fall in the same order-of-magnitude band (attempt 1 semantics);
		// assert via the same bound check rather than exact equality (jitter is random).
		lo := time.Duration(10 * 0.8 * float64(time.Second))
		hi := time.Duration(10 * 1.2 * float64(time.Second))
		for _, got := range []time.Duration{retryDelayWithJitter(10, 1), retryDelayWithJitter(10, 0)} {
			if got < lo || got > hi {
				t.Fatalf("got %v, want within [%v, %v]", got, lo, hi)
			}
		}
	})
}

func pow2(exp int) float64 {
	result := 1.0
	for i := 0; i < exp; i++ {
		result *= 2
	}
	return result
}

func TestIsNonRetriable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"context.Canceled", context.Canceled, true},
		{"plain error", errors.New("boom"), false},
		{"unknown step type", errUnknownStepType{typ: "bogus"}, true},
		{"invalid delay seconds", errInvalidDelaySeconds{}, true},
		{"HTTP 500 is retriable", &HTTPStatusError{StatusCode: 500}, false},
		{"HTTP 429 is retriable (rate limit)", &HTTPStatusError{StatusCode: 429}, false},
		{"HTTP 404 is non-retriable", &HTTPStatusError{StatusCode: 404}, true},
		{"HTTP 400 is non-retriable", &HTTPStatusError{StatusCode: 400}, true},
		{"HTTP 200 (not actually an error case, but exercise the boundary)", &HTTPStatusError{StatusCode: 200}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNonRetriable(tt.err); got != tt.want {
				t.Errorf("isNonRetriable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
