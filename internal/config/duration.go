package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a duration of the config files: a Go duration ("10m", "1h30m")
// or a number of days or years ("90d", "1y"). "0" is zero.
type Duration time.Duration

// ParseDuration reads a Duration.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	for _, u := range []struct {
		suffix string
		d      time.Duration
	}{{"d", 24 * time.Hour}, {"y", 365 * 24 * time.Hour}, {"w", 7 * 24 * time.Hour}} {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.Atoi(strings.TrimSuffix(s, u.suffix))
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			return time.Duration(n) * u.d, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q: want for example 30s, 10m, 2h, 90d or 1y", s)
	}
	return d, nil
}

// String gives the duration in the form of the files.
func (d Duration) String() string { return FormatDuration(time.Duration(d)) }

// FormatDuration gives d as days or years when it is a whole number of them,
// else as a Go duration.
func FormatDuration(d time.Duration) string {
	day := 24 * time.Hour
	switch {
	case d == 0:
		return "0"
	case d%(365*day) == 0:
		return fmt.Sprintf("%dy", d/(365*day))
	case d%day == 0:
		return fmt.Sprintf("%dd", d/day)
	}
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h") || strings.HasSuffix(s, "m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// UnmarshalYAML reads a Duration.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes a Duration.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// IsZero reports a zero duration, for omitempty.
func (d Duration) IsZero() bool { return d == 0 }
