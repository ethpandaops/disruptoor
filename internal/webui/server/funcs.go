package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"reflect"
	"sort"
	"strings"
	"time"
)

// GetTemplateFuncs returns the small set of helpers disruptoor templates need.
// Pruned from spamoor's set to what we actually use; rebuild it as we add more
// pages.
func GetTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"add":         func(i, j int) int { return i + j },
		"sub":         func(i, j int) int { return i - j },
		"json":        jsonPretty,
		"jsonCompact": jsonCompact,
		"joinStrings": func(sep string, ss []string) string { return strings.Join(ss, sep) },
		"defaultStr": func(def, s string) string {
			if s == "" {
				return def
			}
			return s
		},
		"sortedKeys":        sortedKeys,
		"sortedKeysFromAny": sortedKeysFromAny,
		"formatTimeDiff":    formatTimeDiff,
		"formatTimeRFC":     func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"orDash": func(s string) string {
			if s == "" {
				return "—"
			}
			return s
		},
		"orZero": func(i int) string {
			if i == 0 {
				return "0"
			}
			return fmt.Sprint(i)
		},
	}
}

// jsonPretty marshals v as 2-space indented JSON. Used in the state editor.
func jsonPretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("<json error: %v>", err)
	}
	return string(b)
}

// jsonCompact marshals v as a single-line JSON string. Used inside table cells
// where we want copyable but compact label data.
func jsonCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<json error: %v>", err)
	}
	return string(b)
}

// sortedKeys returns the keys of m sorted alphabetically. Stable rendering of
// label maps in templates.
func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedKeysFromAny accepts any map keyed by string and returns its keys
// sorted. Used in templates where a value-typed map (e.g. map[string]string for
// container labels) isn't compatible with the typed sortedKeys helper.
func sortedKeysFromAny(v any) []string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Map {
		return nil
	}
	if rv.Type().Key().Kind() != reflect.String {
		return nil
	}
	keys := make([]string, 0, rv.Len())
	for _, k := range rv.MapKeys() {
		keys = append(keys, k.String())
	}
	sort.Strings(keys)
	return keys
}

// formatTimeDiff returns a short "5s ago" / "in 1m" string. Same idea as the
// spamoor helper but trimmed to seconds/minutes/hours/days. Empty time → "".
func formatTimeDiff(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	abs := d
	if abs < 0 {
		abs = -abs
	}
	var s string
	switch {
	case abs < time.Second:
		return "now"
	case abs < time.Minute:
		s = fmt.Sprintf("%ds", int(abs.Seconds()))
	case abs < time.Hour:
		s = fmt.Sprintf("%dm", int(abs.Minutes()))
	case abs < 24*time.Hour:
		s = fmt.Sprintf("%dh", int(abs.Hours()))
	default:
		s = fmt.Sprintf("%dd", int(abs.Hours()/24))
	}
	if d < 0 {
		return "in " + s
	}
	return s + " ago"
}
