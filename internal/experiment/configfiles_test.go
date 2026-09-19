package experiment

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// configDir is the version-controlled experiment specification directory.
const configDir = "../../experiments/configs"

// TestShippedConfigsLoad verifies that every shipped configuration parses and
// validates.
//
// This test exists because of a real failure: the config structs originally
// carried `yaml:` tags, which sigs.k8s.io/yaml ignores (it converts YAML to JSON
// and uses encoding/json, so only `json:` tags apply). Every shipped config
// silently failed to populate its fields. A test that merely constructed configs
// in Go would never have caught it — only loading the actual files does.
func TestShippedConfigsLoad(t *testing.T) {
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatalf("read %s: %v", configDir, err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		found++
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(configDir, name))
			if err != nil {
				t.Fatal(err)
			}
			var cfg Config
			if err := yaml.Unmarshal(b, &cfg); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			// Validation alone is not enough: a config whose keys did not match
			// would fail validation for the wrong reason, or pass while silently
			// using defaults. Assert that the fields a config file visibly sets
			// actually arrived.
			if cfg.Name == "" {
				t.Error("name did not load")
			}
			if len(cfg.CPUStrategies) == 0 || len(cfg.MemoryStrategies) == 0 {
				t.Error("strategies did not load")
			}
			if len(cfg.ObservationWindows) == 0 {
				t.Error("observation windows did not load")
			}
			if cfg.Step.D() <= 0 {
				t.Error("step did not load")
			}
			if cfg.TraceDuration.D() <= 0 {
				t.Error("trace duration did not load")
			}
			if cfg.EvaluationHorizon.D() <= 0 {
				t.Error("evaluation horizon did not load")
			}
			if cfg.Seeds < 1 {
				t.Error("seeds did not load")
			}
			if cfg.Conditions() < 1 {
				t.Error("the matrix expands to no conditions")
			}
			t.Logf("%s: %d conditions", cfg.Name, cfg.Conditions())
		})
	}
	if found == 0 {
		t.Fatalf("no configuration files found in %s", configDir)
	}
}

// Ablation configs must share their base seed with the experiment they are
// compared against, or the comparison would confound the ablated component with
// a different draw of traces.
func TestAblationConfigsShareSeedsWithMain(t *testing.T) {
	load := func(name string) Config {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(configDir, name))
		if err != nil {
			t.Fatal(err)
		}
		var c Config
		if err := yaml.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	main := load("main.yaml")
	for _, name := range []string{
		"ablation_no_oom_protection.yaml",
		"ablation_unified_strategy.yaml",
		"ablation_no_gates.yaml",
	} {
		a := load(name)
		if a.BaseSeed != main.BaseSeed {
			t.Errorf("%s: base_seed %d differs from main's %d; the comparison would confound the ablation with a different set of traces",
				name, a.BaseSeed, main.BaseSeed)
		}
		if a.TraceDuration != main.TraceDuration || a.Step != main.Step {
			t.Errorf("%s: trace geometry differs from main (%s/%s vs %s/%s)",
				name, a.TraceDuration, a.Step, main.TraceDuration, main.Step)
		}
		if a.EvaluationHorizon != main.EvaluationHorizon {
			t.Errorf("%s: evaluation horizon %s differs from main's %s", name, a.EvaluationHorizon, main.EvaluationHorizon)
		}
	}
}
