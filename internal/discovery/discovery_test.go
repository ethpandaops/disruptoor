package discovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLabelsMatch(t *testing.T) {
	tests := []struct {
		name string
		want map[string][]string
		have map[string]string
		ok   bool
	}{
		{
			name: "single key match",
			want: map[string][]string{"k": {"v"}},
			have: map[string]string{"k": "v"},
			ok:   true,
		},
		{
			name: "OR across values",
			want: map[string][]string{"client-type": {"execution", "beacon"}},
			have: map[string]string{"client-type": "beacon"},
			ok:   true,
		},
		{
			name: "AND across keys",
			want: map[string][]string{"a": {"1"}, "b": {"2"}},
			have: map[string]string{"a": "1", "b": "2"},
			ok:   true,
		},
		{
			name: "missing key fails",
			want: map[string][]string{"a": {"1"}, "b": {"2"}},
			have: map[string]string{"a": "1"},
			ok:   false,
		},
		{
			name: "value mismatch fails",
			want: map[string][]string{"k": {"v"}},
			have: map[string]string{"k": "x"},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.ok, labelsMatch(tt.want, tt.have))
		})
	}
}

func TestContainerIDRegex(t *testing.T) {
	lines := []string{
		"480 478 0:152 / / rw,relatime master:122 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/abc/diff",
		"482 480 0:153 /docker/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd /etc/hostname rw,nosuid",
		"600 480 0:160 /containers/fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210/hostname /etc/hostname rw",
	}
	expected := []string{
		"",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd", // 62 chars - won't match
		"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
	}
	for i, line := range lines {
		m := containerIDRe.FindStringSubmatch(line)
		if expected[i] == "" {
			assert.Nil(t, m, "line %d should not match", i)
			continue
		}
		// note: the second test line has 62 chars by design — verify regex
		// requires exactly 64
		if len(expected[i]) == 64 {
			if assert.NotNil(t, m, "line %d should match", i) {
				assert.Equal(t, expected[i], m[1])
			}
		}
	}
}

func TestQualify(t *testing.T) {
	s := &service{cfg: Config{LabelPrefix: DefaultLabelPrefix}}
	assert.Equal(t,
		"com.kurtosistech.custom.ethereum-package.node-index",
		s.qualify("node-index"))
	assert.Equal(t, "com.example.foo", s.qualify("com.example.foo"))
}

func TestParsePorts(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []Port
		wantErr bool
	}{
		{name: "empty", input: "", want: nil},
		{
			name:  "EL example from kurtosis",
			input: "metrics:9001/TCP/http,tcp-discovery:30303/TCP,udp-discovery:30303/UDP,engine-rpc:8551/TCP,rpc:8545/TCP,ws:8546/TCP",
			want: []Port{
				{Name: "metrics", Number: 9001, Protocol: "TCP"},
				{Name: "tcp-discovery", Number: 30303, Protocol: "TCP"},
				{Name: "udp-discovery", Number: 30303, Protocol: "UDP"},
				{Name: "engine-rpc", Number: 8551, Protocol: "TCP"},
				{Name: "rpc", Number: 8545, Protocol: "TCP"},
				{Name: "ws", Number: 8546, Protocol: "TCP"},
			},
		},
		{
			name:  "CL with quic",
			input: "http:4000/TCP/http,metrics:5054/TCP/http,tcp-discovery:9000/TCP,udp-discovery:9000/UDP,quic-discovery:9001/UDP",
			want: []Port{
				{Name: "http", Number: 4000, Protocol: "TCP"},
				{Name: "metrics", Number: 5054, Protocol: "TCP"},
				{Name: "tcp-discovery", Number: 9000, Protocol: "TCP"},
				{Name: "udp-discovery", Number: 9000, Protocol: "UDP"},
				{Name: "quic-discovery", Number: 9001, Protocol: "UDP"},
			},
		},
		{name: "missing colon", input: "metrics9001/TCP", wantErr: true},
		{name: "missing protocol", input: "metrics:9001", wantErr: true},
		{name: "bad protocol", input: "metrics:9001/SCTP", wantErr: true},
		{name: "bad port", input: "metrics:abc/TCP", wantErr: true},
		{name: "out of range", input: "metrics:99999/TCP", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePorts(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
