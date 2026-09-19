package main

import (
	"io"

	"sigs.k8s.io/yaml"
)

type yamlEncoder struct{ w io.Writer }

func newYAMLEncoder(w io.Writer) *yamlEncoder { return &yamlEncoder{w: w} }

// Encode writes v as YAML. It exists so that --print-config can show the fully
// resolved configuration, which is the fastest way to answer "why is the
// optimizer behaving like that" without reading the code.
func (e *yamlEncoder) Encode(v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = e.w.Write(b)
	return err
}
