package notification

import (
	"testing"
	"time"
)

func TestNextBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: 10 * time.Second},
		{attempt: 1, want: 10 * time.Second},
		{attempt: 2, want: 20 * time.Second},
		{attempt: 3, want: 40 * time.Second},
		{attempt: 99, want: 5 * time.Minute},
	}

	for _, tt := range tests {
		got := NextBackoff(tt.attempt)
		if got != tt.want {
			t.Fatalf("NextBackoff(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}
