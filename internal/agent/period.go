package agent

import (
	"fmt"
	"strconv"
	"time"
)

// ParsePeriod converts a fixed-width HHMMSS period string into a time.Duration.
//
// The Progress Report defines the GPUCheckpoint .spec.period field as a
// six-digit "000000" value encoding hours, minutes and seconds. An empty string
// or "000000" means "no period" (a single one-shot checkpoint).
//
//	"000030" -> 30 * time.Second
//	"000500" -> 5  * time.Minute
//	"010000" -> 1  * time.Hour
func ParsePeriod(p string) (time.Duration, error) {
	if p == "" {
		return 0, nil
	}
	if len(p) != 6 {
		return 0, fmt.Errorf("period %q must be exactly 6 digits (HHMMSS)", p)
	}
	hh, err := strconv.Atoi(p[0:2])
	if err != nil {
		return 0, fmt.Errorf("invalid hours in period %q: %w", p, err)
	}
	mm, err := strconv.Atoi(p[2:4])
	if err != nil {
		return 0, fmt.Errorf("invalid minutes in period %q: %w", p, err)
	}
	ss, err := strconv.Atoi(p[4:6])
	if err != nil {
		return 0, fmt.Errorf("invalid seconds in period %q: %w", p, err)
	}
	if mm > 59 || ss > 59 {
		return 0, fmt.Errorf("invalid period %q: minutes/seconds must be < 60", p)
	}
	return time.Duration(hh)*time.Hour +
		time.Duration(mm)*time.Minute +
		time.Duration(ss)*time.Second, nil
}

// IsOneShot reports whether the period represents a single checkpoint.
func IsOneShot(p string) bool {
	d, err := ParsePeriod(p)
	return err != nil || d == 0
}
