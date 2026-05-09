// Package config loads disruptoor desired-state from a YAML or JSON file.
//
// The wire format is identical to PUT /v1/state (see internal/state). Format
// detection is by file extension: .yaml/.yml → YAML, .json → JSON. YAML is
// routed through json.Marshal so Selector's custom JSON unmarshaller — which
// accepts the literal string "all" or a label-match object — applies to YAML
// inputs as well.
//
// Validation runs before Load returns; callers can apply the result without
// re-validating.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/disruptoor/internal/state"
)

// Format identifies the on-disk encoding of a config blob.
type Format string

const (
	FormatYAML Format = "yaml"
	FormatJSON Format = "json"
)

// Load reads and validates desired state from path. The file format is
// autodetected from the extension. Returns the parsed state or a wrapped
// error; the latter indicates the file should be treated as unusable
// (callers exit non-zero rather than start with partial state).
func Load(path string) (state.State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return state.State{}, fmt.Errorf("read %s: %w", path, err)
	}
	format, err := DetectFormat(path)
	if err != nil {
		return state.State{}, err
	}
	return Decode(data, format)
}

// DetectFormat returns the Format implied by the file extension. Unknown or
// missing extensions are an error: we do not guess based on content because
// a YAML file mistyped as .yml-typo would silently fall through to JSON
// and fail with a confusing parse error.
func DetectFormat(path string) (Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return FormatYAML, nil
	case ".json":
		return FormatJSON, nil
	default:
		return "", fmt.Errorf("unsupported config extension %q (want .yaml, .yml, or .json)",
			filepath.Ext(path))
	}
}

// Decode parses and validates state from raw bytes in the given format.
func Decode(data []byte, format Format) (state.State, error) {
	var s state.State
	switch format {
	case FormatJSON:
		if err := strictJSONUnmarshal(data, &s); err != nil {
			return state.State{}, fmt.Errorf("parse json: %w", err)
		}
	case FormatYAML:
		// Selector's UnmarshalJSON is the source of truth. Convert YAML to a
		// JSON-compatible value tree and reuse it.
		var generic any
		if err := yaml.Unmarshal(data, &generic); err != nil {
			return state.State{}, fmt.Errorf("parse yaml: %w", err)
		}
		jsonBytes, err := json.Marshal(normalizeYAML(generic))
		if err != nil {
			return state.State{}, fmt.Errorf("yaml to json: %w", err)
		}
		if err := strictJSONUnmarshal(jsonBytes, &s); err != nil {
			return state.State{}, fmt.Errorf("decode yaml as state: %w", err)
		}
	default:
		return state.State{}, fmt.Errorf("unsupported format %q", format)
	}
	if err := s.Validate(); err != nil {
		return state.State{}, fmt.Errorf("validate: %w", err)
	}
	return s, nil
}

// strictJSONUnmarshal rejects unknown fields so typos in the config become
// errors instead of silently doing nothing. Selectors are exempt — their
// keys are dynamic label names, not a fixed schema — but the top-level
// State and Partition/Shaping shapes are.
func strictJSONUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// normalizeYAML coerces the value tree returned by yaml.v3 into something
// json.Marshal accepts. yaml.v3 with string keys returns map[string]any
// directly, but mixed-key maps surface as map[any]any; this function flattens
// either case recursively.
func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeYAML(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			out = append(out, normalizeYAML(val))
		}
		return out
	default:
		return v
	}
}
