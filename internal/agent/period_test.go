package agent

import (
	"testing"
	"time"
)

func TestParsePeriod(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"000000", 0, false},
		{"000030", 30 * time.Second, false},
		{"000500", 5 * time.Minute, false},
		{"010000", time.Hour, false},
		{"013015", time.Hour + 30*time.Minute + 15*time.Second, false},
		{"240000", 24 * time.Hour, false},
		{"00500", 0, true},   // wrong width
		{"0005000", 0, true}, // wrong width
		{"00ab00", 0, true},  // non-numeric
		{"006000", 0, true},  // minutes >= 60
		{"000099", 0, true},  // seconds >= 60
	}
	for _, c := range cases {
		got, err := ParsePeriod(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParsePeriod(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePeriod(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePeriod(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsOneShot(t *testing.T) {
	if !IsOneShot("000000") {
		t.Error(`"000000" should be one-shot`)
	}
	if !IsOneShot("") {
		t.Error(`"" should be one-shot`)
	}
	if IsOneShot("000500") {
		t.Error(`"000500" should not be one-shot`)
	}
}
