package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeJSON(t *testing.T) {
	body := []byte(`{
      "partitions": [
        {
          "name": "split",
          "groups": [
            {"id": "alpha"},
            {"id": "bravo"}
          ]
        }
      ]
    }`)

	s, err := Decode(body, FormatJSON)
	require.NoError(t, err)
	require.Len(t, s.Partitions, 1)
	assert.Equal(t, "split", s.Partitions[0].Name)
	require.Len(t, s.Partitions[0].Groups, 2)
	assert.Equal(t, []string{"alpha"}, s.Partitions[0].Groups[0].Match["id"])
	assert.Equal(t, []string{"bravo"}, s.Partitions[0].Groups[1].Match["id"])
}

func TestDecodeYAML(t *testing.T) {
	body := []byte(`
partitions:
  - name: split
    groups:
      - {id: alpha}
      - {id: bravo}
    scope: [cl_p2p, el_p2p]
shaping:
  - name: slow
    target:
      id: alpha
    scope: [include_control]
    delay: 200ms
    jitter: 50ms
`)

	s, err := Decode(body, FormatYAML)
	require.NoError(t, err)
	require.Len(t, s.Partitions, 1)
	require.Len(t, s.Shaping, 1)
	assert.Equal(t, "split", s.Partitions[0].Name)
	assert.Equal(t, []string{"cl_p2p", "el_p2p"}, s.Partitions[0].Scope)
	assert.Equal(t, "200ms", s.Shaping[0].Delay)
	assert.Equal(t, "50ms", s.Shaping[0].Jitter)
}

func TestDecodeYAMLAllSelector(t *testing.T) {
	body := []byte(`
shaping:
  - name: jitter-everything
    target: all
    scope: [include_control]
    delay: 50ms
`)
	s, err := Decode(body, FormatYAML)
	require.NoError(t, err)
	require.Len(t, s.Shaping, 1)
	assert.True(t, s.Shaping[0].Target.All)
}

func TestDecodeRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		body string
		fmt  Format
	}{
		{
			name: "duplicate name",
			fmt:  FormatYAML,
			body: `
partitions:
  - name: dup
    groups:
      - {id: alpha}
      - {id: bravo}
shaping:
  - name: dup
    target: all
    scope: [include_control]
    delay: 10ms
`,
		},
		{
			name: "unknown field on partition",
			fmt:  FormatJSON,
			body: `{"partitions":[{"name":"x","groups":[{"id":"a"},{"id":"b"}],"weather":"sunny"}]}`,
		},
		{
			name: "shaping without delay/loss/bandwidth",
			fmt:  FormatYAML,
			body: `
shaping:
  - name: empty
    target: all
    scope: [include_control]
`,
		},
		{
			name: "partition with one group",
			fmt:  FormatYAML,
			body: `
partitions:
  - name: solo
    groups:
      - {id: alpha}
`,
		},
		{
			name: "malformed yaml",
			fmt:  FormatYAML,
			body: `partitions: [unbalanced`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.body), tt.fmt)
			assert.Error(t, err)
		})
	}
}

func TestDetectFormat(t *testing.T) {
	tests := map[string]struct {
		path    string
		want    Format
		wantErr bool
	}{
		"yaml":    {path: "/etc/disruptoor.yaml", want: FormatYAML},
		"yml":     {path: "init.yml", want: FormatYAML},
		"json":    {path: "init.json", want: FormatJSON},
		"upper":   {path: "init.YAML", want: FormatYAML},
		"unknown": {path: "init.toml", wantErr: true},
		"none":    {path: "init", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := DetectFormat(tt.path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "init.yaml")
	body := []byte(`
partitions:
  - name: split
    groups:
      - {id: alpha}
      - {id: bravo}
`)
	require.NoError(t, os.WriteFile(path, body, 0o600))

	s, err := Load(path)
	require.NoError(t, err)
	require.Len(t, s.Partitions, 1)
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/disruptoor-test/missing.yaml")
	assert.Error(t, err)
}
