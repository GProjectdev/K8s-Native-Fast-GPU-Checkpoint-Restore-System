package agent

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	cases := map[string]time.Duration{
		"":    0,
		"30s": 30 * time.Second,
		"5m":  5 * time.Minute,
		"1h":  time.Hour,
	}
	for in, want := range cases {
		got, err := ParseSchedule(in)
		if err != nil {
			t.Fatalf("ParseSchedule(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseSchedule(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseSchedule("nonsense"); err == nil {
		t.Fatalf("expected error for invalid schedule")
	}
}
