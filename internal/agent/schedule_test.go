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

func TestNextRun(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	// empty => one-shot
	if _, rec, err := NextRun("", t0); err != nil || rec {
		t.Fatalf(`NextRun("") = rec %v err %v, want rec=false`, rec, err)
	}
	// duration
	if nx, rec, err := NextRun("5m", t0); err != nil || !rec || !nx.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf(`NextRun("5m") = %v rec %v err %v`, nx, rec, err)
	}
	// standard cron: every 2 hours -> next is 12:00
	if nx, rec, err := NextRun("0 */2 * * *", t0); err != nil || !rec || nx.Hour() != 12 {
		t.Fatalf(`NextRun("0 */2 * * *") = %v rec %v err %v`, nx, rec, err)
	}
	// descriptor
	if _, rec, err := NextRun("@hourly", t0); err != nil || !rec {
		t.Fatalf(`NextRun("@hourly") rec %v err %v`, rec, err)
	}
	// invalid
	if _, _, err := NextRun("nonsense", t0); err == nil {
		t.Fatalf("expected error for invalid schedule")
	}
}
