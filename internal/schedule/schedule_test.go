package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestParseAndNextHonourTheTimeZone(t *testing.T) {
	spec, err := Parse("30 6 * * *", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	// 06:30 Berlin in July is 04:30 UTC.
	got := spec.Next(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	want := time.Date(2026, 7, 2, 4, 30, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("next = %s, want %s", got, want)
	}
	utc, _ := Parse("30 6 * * *", "")
	if got := utc.Next(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 7, 2, 6, 30, 0, 0, time.UTC)) {
		t.Errorf("utc next = %s", got)
	}
	every, _ := Parse("@every 90s", "UTC")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := every.Next(base); !got.Equal(base.Add(90 * time.Second)) {
		t.Errorf("@every next = %s", got)
	}
	daily, _ := Parse("@daily", "UTC")
	if got := daily.Next(base.Add(time.Hour)); !got.Equal(base.Add(24 * time.Hour)) {
		t.Errorf("@daily next = %s", got)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := []struct{ expr, tz, want string }{
		{"", "UTC", "required"},
		{"* * * *", "UTC", "expected exactly 5 fields"},
		{"61 * * * *", "UTC", "above maximum"},
		{"* * * * * *", "UTC", "expected exactly 5 fields"},
		{"@every 1x", "UTC", "failed to parse duration"},
		{"TZ=UTC * * * * *", "", "timezone field"},
		{"* * * * *", "Mars/Olympus", "unknown"},
	}
	for _, c := range cases {
		_, err := Parse(c.expr, c.tz)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q %q: err = %v, want %q", c.expr, c.tz, err, c.want)
		}
	}
}
