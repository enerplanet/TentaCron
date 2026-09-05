// Package schedule parses cron expressions and computes run times in a time
// zone. It is the only place the cron library is used, so its vocabulary
// (five fields plus @descriptors) is the contract the API documents.
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// parser accepts the standard five fields (minute hour day-of-month month
// day-of-week) and the descriptors @hourly, @daily, @weekly, @monthly,
// @yearly and @every <duration>.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Spec is a parsed schedule bound to a location.
type Spec struct {
	sched cron.Schedule
	loc   *time.Location
}

// Parse validates a cron expression and a time zone name ("" means UTC).
// A TZ= or CRON_TZ= prefix inside the expression is refused: the zone is a
// separate field.
func Parse(expr, timezone string) (Spec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Spec{}, errors.New("cron expression is required")
	}
	if strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") {
		return Spec{}, errors.New("set the time zone in the timezone field, not inside the expression")
	}
	loc, err := Location(timezone)
	if err != nil {
		return Spec{}, err
	}
	sched, err := parser.Parse(expr)
	if err != nil {
		return Spec{}, fmt.Errorf("cron %q: %w", expr, err)
	}
	if ss, ok := sched.(*cron.SpecSchedule); ok {
		ss.Location = loc
	}
	return Spec{sched: sched, loc: loc}, nil
}

// Location resolves a time zone name; "" is UTC.
func Location(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("timezone %q is unknown", name)
	}
	return loc, nil
}

// Next returns the first run strictly after t, in UTC.
func (s Spec) Next(t time.Time) time.Time {
	return s.sched.Next(t.In(s.loc)).UTC()
}
