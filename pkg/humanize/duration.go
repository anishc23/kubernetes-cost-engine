// Package humanize provides a time.Duration that round-trips through YAML and
// JSON in human-readable form.
//
// This exists because sigs.k8s.io/yaml converts YAML to JSON and then uses
// encoding/json, which marshals a time.Duration as an integer count of
// nanoseconds and refuses to unmarshal "24h". Configuration files are read by
// people, and "observationWindow: 86400000000000" is not a configuration file
// anyone should have to write or review.
package humanize

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration wraps time.Duration with string-based JSON and YAML encoding.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration in Go's standard form.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON writes the duration as a string, so that a config round-tripped
// through the tool remains readable.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string ("24h", "30s", "1h30m") or a
// number of nanoseconds.
//
// Numbers are accepted so that a file written by MarshalJSON in some older form,
// or produced by a generator that does not know about this type, still loads
// rather than failing at startup.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*d = Duration(n)
		return nil
	}
	return fmt.Errorf("duration must be a string such as \"24h\" or a number of nanoseconds, got %s", b)
}

// Durations is a slice of Duration with a helper for converting to the standard
// type, which is what the matrix expansion consumes.
type Durations []Duration

// Std converts to a slice of time.Duration.
func (ds Durations) Std() []time.Duration {
	out := make([]time.Duration, len(ds))
	for i, d := range ds {
		out[i] = time.Duration(d)
	}
	return out
}
