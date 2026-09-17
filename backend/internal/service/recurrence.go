package service

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// NextRunAt computes the next UTC instant a recurring scheduled task should
// fire, strictly AFTER the given `after` moment. It is a pure function
// (no DB, no globals beyond time.LoadLocation) so it is trivially unit
// testable — the re-arm logic in ScheduledTaskService.Finish leans on it.
//
// Semantics:
//   - recurrence == "once" (or unknown) → returns the zero time and nil
//     error: a one-shot task has no "next" occurrence.
//   - "daily"  → the next day (or today) whose recur_time (HH:MM, wall-clock
//     in tz) is strictly after `after`.
//   - "weekly" → the next such moment that ALSO lands on a weekday listed in
//     recur_days (CSV of 0-6, 0=Sunday, matching JS getDay()).
//
// The wall-clock time is interpreted in `tz` (an IANA name); if tz is empty
// or fails to load (scratch containers may ship without system tzdata — see
// the `import _ "time/tzdata"` in cmd/server/main.go which mitigates this)
// the function falls back to UTC rather than erroring, so a bad tz never
// wedges the scheduler. The returned time is always in UTC.
//
// recur_time is validated; a malformed value returns an error. weekly with
// an empty/invalid recur_days also errors. Callers (Create / Update) surface
// these as 400s; Finish treats a zero return as "cannot re-arm" and degrades
// the row to terminal + inactive.
func NextRunAt(recurrence, recurTime, recurDays, tz string, after time.Time) (time.Time, error) {
	switch recurrence {
	case "", model.SchedRecurOnce:
		return time.Time{}, nil
	case model.SchedRecurDaily, model.SchedRecurWeekly:
		// handled below
	default:
		return time.Time{}, fmt.Errorf("unknown recurrence %q", recurrence)
	}

	hour, minute, err := parseRecurTime(recurTime)
	if err != nil {
		return time.Time{}, err
	}

	var allowedDays map[int]bool
	if recurrence == model.SchedRecurWeekly {
		allowedDays, err = parseRecurDays(recurDays)
		if err != nil {
			return time.Time{}, err
		}
		if len(allowedDays) == 0 {
			return time.Time{}, fmt.Errorf("weekly recurrence requires at least one recur day")
		}
	}

	loc := time.UTC
	if tz != "" {
		if l, lerr := time.LoadLocation(tz); lerr == nil {
			loc = l
		}
	}

	// Work in the target location so wall-clock HH:MM lands on the right
	// civil day (and DST is handled by time.Date normalization).
	base := after.In(loc)
	// Candidate on the same civil day as `after`, at the requested HH:MM.
	candidate := time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, loc)
	// Advance day-by-day until the candidate is strictly after `after` AND
	// (weekly) on an allowed weekday. Cap at 8 iterations: daily needs at
	// most 1 bump, weekly at most 7 to sweep a full week — 8 is a safe
	// upper bound that also guards against a pathological DST/tz edge.
	for i := 0; i < 8; i++ {
		if candidate.After(after) {
			if recurrence == model.SchedRecurDaily || allowedDays[int(candidate.Weekday())] {
				return candidate.UTC(), nil
			}
		}
		candidate = candidate.AddDate(0, 0, 1)
	}
	// Should be unreachable given the caps above; return an error so the
	// caller can fail loudly rather than silently scheduling nothing.
	return time.Time{}, fmt.Errorf("could not compute next run within 8 days (recurrence=%s time=%s days=%s tz=%s)", recurrence, recurTime, recurDays, tz)
}

// parseRecurTime parses an "HH:MM" 24-hour wall-clock string into hour and
// minute. Rejects out-of-range values so a typo doesn't silently roll over
// into the next day via time.Date normalization.
func parseRecurTime(s string) (hour, minute int, err error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid recur_time %q (want HH:MM)", s)
	}
	hour, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("invalid recur_time hour in %q (want 00-23)", s)
	}
	minute, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid recur_time minute in %q (want 00-59)", s)
	}
	return hour, minute, nil
}

// parseRecurDays parses a CSV of weekday numbers (0-6, 0=Sunday) into a set.
// Blank entries are skipped; out-of-range or non-numeric values error.
func parseRecurDays(s string) (map[int]bool, error) {
	out := map[int]bool{}
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		d, err := strconv.Atoi(raw)
		if err != nil || d < 0 || d > 6 {
			return nil, fmt.Errorf("invalid recur_days entry %q (want 0-6)", raw)
		}
		out[d] = true
	}
	return out, nil
}
