// Package discovery resolves label-based selectors against the live Docker
// daemon. It scopes every query to a single enclave, learnt at startup by
// reading the disruptoor container's own labels (or supplied via config).
//
// Resolution happens at PUT time, never cached: Kurtosis labels change on
// every enclave run, and a container's IP can change on restart.
package discovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"github.com/ethpandaops/disruptoor/internal/state"
)

// DefaultLabelPrefix is prepended to selector keys that have no dot.
// Kurtosis nests user-supplied labels under com.kurtosistech.custom.*, so
// ethereum-package's own labels show up at this fully-qualified path.
const DefaultLabelPrefix = "com.kurtosistech.custom.ethereum-package."

// DefaultEnclaveLabelKey is Kurtosis's enclave identifier label. Override at
// startup if a future Kurtosis release renames it.
const DefaultEnclaveLabelKey = "com.kurtosistech.enclave-id"

// ContainerTypeLabel and userService scope discovery to participant
// containers, never the Kurtosis API container, log collector, or other
// infrastructure that lives on the same enclave network.
const (
	ContainerTypeLabel = "com.kurtosistech.container-type"
	userServiceType    = "user-service"
)

// privateIPLabel and serviceNameLabel are Kurtosis-set conveniences. Every
// participant has them; they save us a NetworkSettings traversal and give a
// human-friendly name without the random GUID suffix.
const (
	privateIPLabel   = "com.kurtosistech.private-ip"
	serviceNameLabel = "com.kurtosistech.id"
	portsLabel       = "com.kurtosistech.ports"
)

// Config controls discovery behaviour. Zero values pick reasonable defaults.
type Config struct {
	// EnclaveLabelKey is the Docker label key whose value identifies the
	// enclave. Defaults to DefaultEnclaveLabelKey.
	EnclaveLabelKey string
	// EnclaveLabelValue scopes every query. If empty, Service.Start tries to
	// discover it from its own container labels. If self-discovery fails,
	// the service runs unscoped and logs a loud warning — useful for
	// non-Kurtosis test harnesses.
	EnclaveLabelValue string
	// LabelPrefix is prepended to selector keys without a dot. Defaults to
	// DefaultLabelPrefix.
	LabelPrefix string
	// SelfContainerID overrides self-discovery. Useful for tests.
	SelfContainerID string
}

// Container is a resolved target: enough information to apply rules to it.
type Container struct {
	ID     string
	Name   string
	PID    int
	Labels map[string]string
	IPs    []net.IP
	Ports  []Port
}

// Port is one entry from a Kurtosis container's port catalog.
type Port struct {
	Name     string // e.g. "tcp-discovery", "engine-rpc"
	Number   uint16
	Protocol string // "TCP" or "UDP"
}

// Service resolves selectors into concrete containers.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	// Resolve returns containers matching sel. An empty result is not an
	// error — the caller decides whether that's a problem.
	Resolve(ctx context.Context, sel state.Selector) ([]Container, error)
	// ResolveGroups resolves several selectors at once and reports any
	// container that appears in more than one group.
	ResolveGroups(ctx context.Context, sels []state.Selector) ([][]Container, error)
	// EnclaveID returns the scoping value used for queries. Empty when
	// running unscoped.
	EnclaveID() string
}

// NewService constructs a Service wrapping a Docker client. Pass the logger
// the application configured at its entry point.
func NewService(logger *slog.Logger, cfg Config) (Service, error) {
	if logger == nil {
		return nil, errors.New("logger required")
	}
	if cfg.EnclaveLabelKey == "" {
		cfg.EnclaveLabelKey = DefaultEnclaveLabelKey
	}
	if cfg.LabelPrefix == "" {
		cfg.LabelPrefix = DefaultLabelPrefix
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &service{
		logger: logger.With(slog.String("component", "discovery")),
		cli:    cli,
		cfg:    cfg,
	}, nil
}

type service struct {
	logger    *slog.Logger
	cli       *client.Client
	cfg       Config
	enclaveID string
}

func (s *service) Start(ctx context.Context) error {
	if s.cfg.EnclaveLabelValue != "" {
		s.enclaveID = s.cfg.EnclaveLabelValue
		s.logger.InfoContext(ctx, "scoped to enclave from config",
			slog.String("enclave_id", s.enclaveID))
		return nil
	}

	selfID := s.cfg.SelfContainerID
	if selfID == "" {
		id, err := readSelfContainerID()
		if err != nil {
			s.logger.WarnContext(ctx, "self-container detection failed; running unscoped",
				slog.String("error", err.Error()))
			return nil
		}
		selfID = id
	}

	insp, err := s.cli.ContainerInspect(ctx, selfID)
	if err != nil {
		s.logger.WarnContext(ctx, "self container inspect failed; running unscoped",
			slog.String("self_id", selfID),
			slog.String("error", err.Error()))
		return nil
	}
	if v, ok := insp.Config.Labels[s.cfg.EnclaveLabelKey]; ok && v != "" {
		s.enclaveID = v
		s.logger.InfoContext(ctx, "scoped to enclave from self labels",
			slog.String("enclave_id", v),
			slog.String("label_key", s.cfg.EnclaveLabelKey))
		return nil
	}
	s.logger.WarnContext(ctx, "self container has no enclave label; running unscoped",
		slog.String("self_id", selfID),
		slog.String("label_key", s.cfg.EnclaveLabelKey))
	return nil
}

func (s *service) Stop() error {
	if s.cli != nil {
		return s.cli.Close()
	}
	return nil
}

func (s *service) EnclaveID() string {
	return s.enclaveID
}

func (s *service) Resolve(ctx context.Context, sel state.Selector) ([]Container, error) {
	flt := filters.NewArgs()
	if s.enclaveID != "" {
		flt.Add("label", fmt.Sprintf("%s=%s", s.cfg.EnclaveLabelKey, s.enclaveID))
	}
	flt.Add("label", fmt.Sprintf("%s=%s", ContainerTypeLabel, userServiceType))
	if !sel.All {
		for key := range sel.Match {
			flt.Add("label", s.qualify(key))
		}
	}

	listed, err := s.cli.ContainerList(ctx, container.ListOptions{All: false, Filters: flt})
	if err != nil {
		return nil, fmt.Errorf("docker list: %w", err)
	}

	out := make([]Container, 0, len(listed))
	for i := range listed {
		summary := &listed[i]
		if !sel.All && !labelsMatch(s.qualifyMatch(sel.Match), summary.Labels) {
			continue
		}
		c, err := s.inspect(ctx, summary.ID)
		if err != nil {
			return nil, err
		}
		if s.enclaveID != "" && c.Labels[s.cfg.EnclaveLabelKey] != s.enclaveID {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *service) ResolveGroups(ctx context.Context, sels []state.Selector) ([][]Container, error) {
	out := make([][]Container, len(sels))
	seen := make(map[string]int, 16)
	for i, sel := range sels {
		matched, err := s.Resolve(ctx, sel)
		if err != nil {
			return nil, fmt.Errorf("group %d: %w", i, err)
		}
		for _, c := range matched {
			if prev, dup := seen[c.ID]; dup && prev != i {
				return nil, fmt.Errorf("container %s matches groups %d and %d", c.Name, prev, i)
			}
			seen[c.ID] = i
		}
		out[i] = matched
	}
	return out, nil
}

func (s *service) inspect(ctx context.Context, id string) (Container, error) {
	insp, err := s.cli.ContainerInspect(ctx, id)
	if err != nil {
		return Container{}, fmt.Errorf("inspect %s: %w", id, err)
	}
	if insp.State == nil || insp.State.Pid == 0 {
		return Container{}, fmt.Errorf("container %s has no PID (state %v)", id, insp.State)
	}

	ips := collectIPs(insp.Config.Labels, insp.NetworkSettings)
	name := insp.Config.Labels[serviceNameLabel]
	if name == "" {
		name = strings.TrimPrefix(insp.Name, "/")
	}
	ports, err := parsePorts(insp.Config.Labels[portsLabel])
	if err != nil {
		return Container{}, fmt.Errorf("container %s ports label: %w", id, err)
	}

	return Container{
		ID:     insp.ID,
		Name:   name,
		PID:    insp.State.Pid,
		Labels: insp.Config.Labels,
		IPs:    ips,
		Ports:  ports,
	}, nil
}

func (s *service) qualify(key string) string {
	if strings.Contains(key, ".") {
		return key
	}
	return s.cfg.LabelPrefix + key
}

func (s *service) qualifyMatch(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[s.qualify(k)] = v
	}
	return out
}

// labelsMatch returns true when every key in want appears in have with at
// least one of the listed values.
func labelsMatch(want map[string][]string, have map[string]string) bool {
	for key, allowed := range want {
		got, ok := have[key]
		if !ok {
			return false
		}
		if !containsString(allowed, got) {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// readSelfContainerID inspects /proc/self/mountinfo for a Docker container
// ID. Falls back to /etc/hostname (Docker's default hostname is the short
// container ID).
func readSelfContainerID() (string, error) {
	if id, err := scanMountinfo(); err == nil && id != "" {
		return id, nil
	}
	host, err := os.ReadFile("/etc/hostname")
	if err != nil {
		return "", fmt.Errorf("read /etc/hostname: %w", err)
	}
	id := strings.TrimSpace(string(host))
	if id == "" {
		return "", errors.New("/etc/hostname empty")
	}
	return id, nil
}

// collectIPs prefers the Kurtosis-set private-ip label (one round trip
// saved, deterministic) and falls back to NetworkSettings if absent.
func collectIPs(labels map[string]string, settings *dockertypes.NetworkSettings) []net.IP {
	out := make([]net.IP, 0, 2)
	if v := labels[privateIPLabel]; v != "" {
		if ip := net.ParseIP(v); ip != nil {
			out = append(out, ip)
		}
	}
	if settings == nil {
		return out
	}
	for _, n := range settings.Networks {
		if n == nil || n.IPAddress == "" {
			continue
		}
		ip := net.ParseIP(n.IPAddress)
		if ip == nil || alreadyHas(out, ip) {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func alreadyHas(ips []net.IP, target net.IP) bool {
	for _, ip := range ips {
		if ip.Equal(target) {
			return true
		}
	}
	return false
}

// parsePorts decodes Kurtosis's compact port catalog. Format is
// "name:port/proto[/subproto],...". The optional subproto (http etc.) is
// ignored — we only care about TCP/UDP for filtering.
func parsePorts(label string) ([]Port, error) {
	if label == "" {
		return nil, nil
	}
	entries := strings.Split(label, ",")
	out := make([]Port, 0, len(entries))
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		colon := strings.IndexByte(raw, ':')
		if colon < 0 {
			return nil, fmt.Errorf("entry %q missing ':'", raw)
		}
		name := raw[:colon]
		rest := raw[colon+1:]

		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("entry %q missing protocol", raw)
		}

		port, err := parsePortNumber(parts[0])
		if err != nil {
			return nil, fmt.Errorf("entry %q: %w", raw, err)
		}
		proto := strings.ToUpper(parts[1])
		if proto != "TCP" && proto != "UDP" {
			return nil, fmt.Errorf("entry %q: unknown protocol %q", raw, proto)
		}
		out = append(out, Port{Name: name, Number: port, Protocol: proto})
	}
	return out, nil
}

func parsePortNumber(s string) (uint16, error) {
	var n uint32
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric port %q", s)
		}
		n = n*10 + uint32(r-'0')
		if n > 65535 {
			return 0, fmt.Errorf("port %q out of range", s)
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("empty port")
	}
	return uint16(n), nil
}

var containerIDRe = regexp.MustCompile(`(?:docker[/-]|containers/)([0-9a-f]{64})`)

func scanMountinfo() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if m := containerIDRe.FindStringSubmatch(scanner.Text()); m != nil {
			return m[1], nil
		}
	}
	return "", errors.New("no container id in mountinfo")
}
