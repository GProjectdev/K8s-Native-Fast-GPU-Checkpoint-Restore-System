package agent

import "time"

// ParseSchedule converts a GPUCheckpoint .spec.schedule string into a repeat
// interval. An empty string means one-shot (0). Otherwise it is a Go duration
// string, e.g. "30s", "5m", "1h".
func ParseSchedule(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// IsOneShot reports whether the schedule represents a single checkpoint.
func IsOneShot(s string) bool {
	d, err := ParseSchedule(s)
	return err != nil || d == 0
}
