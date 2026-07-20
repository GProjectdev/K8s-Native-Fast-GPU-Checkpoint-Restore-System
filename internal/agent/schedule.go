package agent

import (
	"strings"
	"time"

	cron "github.com/robfig/cron/v3"
)

// cronParser accepts standard 5-field cron ("0 */2 * * *") and @-descriptors
// ("@hourly", "@daily"). Seconds are intentionally not enabled to keep the
// format familiar.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// ParseSchedule parses a Go duration schedule ("30s", "5m", "1h"); an empty
// string means one-shot (0). It does NOT accept cron — use NextRun for
// cron-aware scheduling. Kept for backward compatibility.
func ParseSchedule(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// looksLikeCron reports whether s is a cron expression rather than a Go
// duration. Descriptors start with '@'; standard cron has 5 space-separated
// fields (a Go duration never contains spaces).
func looksLikeCron(s string) bool {
	if strings.HasPrefix(s, "@") {
		return true
	}
	return len(strings.Fields(s)) >= 5
}

// NextRun returns the next checkpoint time strictly after `from` and whether the
// schedule recurs. Empty schedule => one-shot (recurring=false). Accepts either
// a Go duration ("5m", "1h") or a standard cron expression ("0 */2 * * *", or a
// descriptor like "@hourly").
func NextRun(s string, from time.Time) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, nil
	}
	if looksLikeCron(s) {
		sched, err := cronParser.Parse(s)
		if err != nil {
			return time.Time{}, false, err
		}
		return sched.Next(from), true, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, false, err
	}
	if d <= 0 {
		return time.Time{}, false, nil
	}
	return from.Add(d), true, nil
}

// IsOneShot reports whether the schedule represents a single checkpoint.
func IsOneShot(s string) bool {
	_, recurring, err := NextRun(s, time.Now())
	return err != nil || !recurring
}
