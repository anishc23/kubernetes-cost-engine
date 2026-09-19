package humanize

import (
	"encoding/json"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// The reason this type exists: a duration written the way a person writes it must
// load. Before this type, "30s" in a YAML config was a startup failure.
func TestUnmarshalDurationStrings(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{`"30s"`, 30 * time.Second},
		{`"1m"`, time.Minute},
		{`"24h"`, 24 * time.Hour},
		{`"72h"`, 72 * time.Hour},
		{`"1h30m"`, 90 * time.Minute},
		{`"500ms"`, 500 * time.Millisecond},
	}
	for _, c := range cases {
		var d Duration
		if err := json.Unmarshal([]byte(c.in), &d); err != nil {
			t.Fatalf("Unmarshal(%s): %v", c.in, err)
		}
		if d.D() != c.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", c.in, d.D(), c.want)
		}
	}
}

// Numeric nanoseconds are accepted so that a file produced by an older encoder
// still loads rather than failing at startup.
func TestUnmarshalNanoseconds(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`3600000000000`), &d); err != nil {
		t.Fatal(err)
	}
	if d.D() != time.Hour {
		t.Errorf("got %v, want 1h", d.D())
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	for _, in := range []string{`"not-a-duration"`, `"24 hours"`, `true`, `{}`} {
		var d Duration
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("Unmarshal(%s) should have failed", in)
		}
	}
}

// Round-tripping must preserve readability: a config written back out must still
// be a config someone can review.
func TestRoundTripStaysReadable(t *testing.T) {
	d := Duration(90 * time.Minute)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"1h30m0s"` {
		t.Errorf("marshalled as %s, want a duration string", b)
	}
	var back Duration
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != d {
		t.Errorf("round trip: %v != %v", back, d)
	}
}

// The end-to-end case: YAML through sigs.k8s.io/yaml, which is what the config
// loaders actually use.
func TestYAMLIntegration(t *testing.T) {
	type cfg struct {
		Window Durations `json:"windows"`
		Step   Duration  `json:"step"`
	}
	var c cfg
	if err := yaml.Unmarshal([]byte("windows: [1h, 6h, 24h]\nstep: 30s\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.Step.D() != 30*time.Second {
		t.Errorf("step = %v", c.Step.D())
	}
	want := []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}
	got := c.Window.Std()
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestStdOnEmpty(t *testing.T) {
	if got := Durations(nil).Std(); len(got) != 0 {
		t.Errorf("Std() on nil = %v", got)
	}
}
