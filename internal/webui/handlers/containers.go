package handlers

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/state"
)

// ContainersPage is the payload for /containers.
type ContainersPage struct {
	EnclaveID  string
	Containers []ContainerView
	Error      string
}

// ContainerView is the projection of discovery.Container the template uses.
type ContainerView struct {
	ID         string
	Name       string
	PID        int
	IPs        []string
	Labels     map[string]string
	PortLabels []string // "30303/TCP (tcp-discovery)" style
}

const containersResolveTimeout = 5 * time.Second

// Containers renders the resolved enclave containers page at /containers.
func (h *Handler) Containers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), containersResolveTimeout)
	defer cancel()
	page := &ContainersPage{EnclaveID: h.discovery.EnclaveID()}
	containers, err := h.discovery.Resolve(ctx, state.Selector{All: true})
	if err != nil {
		page.Error = err.Error()
	} else {
		page.Containers = projectContainers(containers)
	}
	data := h.engine.InitPageData(r, "containers", "/containers", "Containers")
	data.Data = page
	h.engine.Render(w, r, []string{"containers/containers.html"}, data)
}

// APIContainers returns the same projection as JSON for consumers (and the
// /resolve endpoint).
func (h *Handler) APIContainers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), containersResolveTimeout)
	defer cancel()
	containers, err := h.discovery.Resolve(ctx, state.Selector{All: true})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enclave_id": h.discovery.EnclaveID(),
		"containers": projectContainers(containers),
	})
}

func projectContainers(in []discovery.Container) []ContainerView {
	views := make([]ContainerView, 0, len(in))
	for _, c := range in {
		ips := make([]string, 0, len(c.IPs))
		for _, ip := range c.IPs {
			ips = append(ips, ip.String())
		}
		ports := make([]string, 0, len(c.Ports))
		for _, p := range c.Ports {
			label := portLabel(p)
			ports = append(ports, label)
		}
		sort.Strings(ports)
		views = append(views, ContainerView{
			ID:         shortID(c.ID),
			Name:       c.Name,
			PID:        c.PID,
			IPs:        ips,
			Labels:     c.Labels,
			PortLabels: ports,
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func portLabel(p discovery.Port) string {
	out := ""
	if p.Name != "" {
		out = p.Name + " "
	}
	out += itoa(int(p.Number)) + "/" + p.Protocol
	return out
}

// itoa is a tiny inline strconv.Itoa to avoid the import for one call site.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
