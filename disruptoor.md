# disruptoor — Design Doc

Status: design / pre-implementation
Audience: an engineer (or agent) picking this up cold

## One-liner

A small, privileged Docker sidecar that applies network disruptions (partitions, latency, loss, bandwidth caps) to other containers in a Kurtosis enclave, controllable via an HTTP API. Designed to be launched as an `additional_service` of the Ethereum package and driven by assertoor.

## Why this exists

Split-network and degraded-link scenarios are hard to test today:

- chaos-mesh works on Kubernetes but is heavy and clunky for local iteration. It has been tried and rejected.
- `cskiraly/ethereum-kurtosis-tc` proves the privileged-sidecar idea works on plain Docker, but is shaping-only (no partitions), out-of-band (separate clone, run by hand), and not integrated with the Ethereum package or assertoor.
- Reproducible CI scenarios for "what happens when group A and group B can't talk during the fork?" don't exist.

disruptoor's job is to be the missing primitive: a Docker-only, locally usable, programmable network controller, exposed through the same patterns users already know (additional services, assertoor tasks).

## Inspiration (do not depend on)

- https://github.com/cskiraly/ethereum-kurtosis-tc — privileged container with `NET_ADMIN` + `/var/run/docker.sock`, finds Kurtosis containers by name/label, applies `tc netem`/`tbf`. Filters by **port** to spare RPC/metrics/engine. CLI-only, no API, no partitions.
- https://github.com/cskiraly/ethereum-fork-testing — wraps the standard Ethereum package with a timing script that calls the tc tool around fork time to degrade traffic. Bash, not declarative, not reproducible without the wrapper repo.

## Goals

1. **Docker-first, locally runnable.** No Kubernetes, no cloud dependency. A developer should be able to run it on a laptop with `kurtosis run`.
2. **Declarative configuration.** Users describe desired network state; the controller reconciles. No imperative add-rule/remove-rule shell scripting from the user side.
3. **Programmable at runtime.** HTTP API for triggering events mid-run (assertoor, spamoor, manual `curl`, future CLIs).
4. **Reproducible.** Same config + same scenario = same network behaviour, deterministically schedulable around epochs/slots.
5. **Ethereum-agnostic core.** disruptoor itself knows nothing about epochs, validators, or forks. It knows containers, ports, and netfilter/tc primitives. The Ethereum-awareness lives in the ethereum-package launcher.
6. **Safe by default.** Never disrupts RPC/engine/metrics/VC↔CL traffic unless the operator explicitly opts in; otherwise tests lose visibility into the system they're testing.

## Non-goals

- **Application-layer manipulation.** No mutating, replaying, dropping, or reordering of specific gossip messages. Operates at L3/L4 only — under libp2p/Noise encryption it cannot see message types.
- **Byzantine client behaviour.** Equivocation, surround voting, withholding belong in modified clients, not here.
- **Per-validator-key partitioning within one VC container.** Granularity is the container; users wanting finer control split keys across multiple VC containers (already supported by the package).
- **Clock skew, disk pressure, CPU starvation, OOM injection.** Out of scope. Network-only.
- **Kubernetes support.** Not a goal. The mechanism (docker.sock + privileged + bridge tc) is Docker-specific.
- **Realistic geographic topology modelling.** We can add latency/loss; we cannot model BGP or multi-region paths beyond what `tc` supports.
- **External-to-enclave services.** Anything not running as a container in the enclave (real relays, public RPC) is unreachable.

## Three-layer architecture

Each layer ships, versions, and is tested independently.

### 1. `disruptoor` (own repo, own Docker image)

Pure mechanism. No knowledge of Ethereum.

- Privileged container, `cap_add: NET_ADMIN`, `/var/run/docker.sock` mounted.
- Discovers target containers via Docker labels (e.g. `com.kurtosistech.*` set by Kurtosis) — but the tool itself accepts generic selectors (label=value, name pattern).
- Backends:
  - **`tc netem` + `tbf`** for shaping (delay, jitter, loss, duplicate, reorder, corrupt, bandwidth).
  - **`iptables` (or `nftables`)** for hard partitions (drop traffic between specific peer IPs).
  - **Connection teardown** via `conntrack -D` / `ss -K` to make partitions take effect on already-open TCP sessions, not just new ones.
- Exposes versioned HTTP API.
- Reusable by anyone running multi-container Docker tests, not just this package.

### 2. ethereum-package integration

Ethereum-aware glue.

- Starlark launcher: `src/disruptoor/disruptoor_launcher.star`.
- New `additional_services` entry: `disruptoor`.
- New `network_params.network_disruption` block — initial state and scheduled events declared in `network_params.yaml`.
- Translation layer: participant name (e.g. `participant_1`, or whatever the user wrote) → container selectors (labels) that disruptoor understands. **This translation lives here, not in disruptoor.**
- Translation of Ethereum-time (`at_epoch`, `at_slot`) → seconds-since-genesis, computed from `genesis_delay` + `seconds_per_slot`.

### 3. assertoor consumption

Ergonomic test authoring.

- New task type, working name `run_network_disruption`.
- POSTs declarative state to the in-enclave disruptoor endpoint.
- Speaks in participant names; the package-level config exposes the name→selector map to assertoor at boot.
- Lives in the assertoor repo, ships independently of disruptoor and the package.

## Mechanism details

### Backends and when each fires

| Disruption          | Backend             | Notes                                             |
|---------------------|---------------------|---------------------------------------------------|
| Latency / jitter    | `tc netem delay`    | Per-container egress + ingress shaping.           |
| Packet loss         | `tc netem loss`     | Per-container, blanket. Not for partitions.       |
| Duplicate / reorder | `tc netem`          | Same.                                             |
| Bandwidth cap       | `tc tbf`            | Per-container.                                    |
| Hard partition      | `iptables -j DROP`  | Per peer-IP pair, in container netns or bridge.   |
| Existing TCP cleanup| `conntrack -D` / `ss -K` | Required when applying a partition over a live session, otherwise drops feel "soft" for tens of seconds. |

### Default port scope (safety)

By default, disruptoor only touches **p2p** ports. From the Kurtosis container labels, the recognised "spare these" ports are at minimum:

- EL: `engine-rpc`, `rpc`, `ws`, `metrics`
- CL: `http`, `metrics`
- VC↔CL channel: HTTP beacon API

Disruption applies to: `tcp-discovery`, `udp-discovery`, `quic-discovery`, libp2p ports, devp2p ports.

Disrupting control ports requires an explicit `scope: include_control` opt-in. Without that, tests keep their visibility into the system.

## API design

Declarative, versioned, idempotent.

### Endpoints

- `PUT /v1/state` — set the entire desired disruption state. Returns when rules are applied.
- `GET /v1/state` — return *applied* state (reflects reality, not last request).
- `POST /v1/state/clear` — heal everything.
- `GET /v1/healthz` — liveness.

### Request shape (sketch)

```json
{
  "partitions": [
    {
      "name": "fork-split",
      "groups": [
        { "node-index": ["1", "2"] },
        { "node-index": ["3", "4"] }
      ],
      "scope": ["cl_p2p", "el_p2p"]
    }
  ],
  "shaping": [
    {
      "name": "dial-up-node",
      "target": { "node-index": "2" },
      "scope": ["include_control"],
      "bandwidth": "1mbit"
    }
  ]
}
```

### Semantics

- **Whole-state replacement.** A `PUT /v1/state` describes the complete desired state. Controller diffs against current and converges. Avoids drift from missed `heal`s.
- **Synchronous apply.** Returns 200 only after kernel rules are in place. Callers (assertoor) can immediately assert without races.
- **Conflict policy.** `GET /v1/state` returns an `ETag`. Callers that send it back as `If-Match` on `PUT /v1/state` get `412 Precondition Failed` if another write landed first.
- **Auth by network.** Controller binds only to the enclave network. No tokens. Opt-in `expose: true` to publish on the host.
- **Stable selectors.** The standalone API accepts label selectors. Higher-level package integrations can translate participant names into these selectors.
- **Validation.** Reject configs at PUT time: same node in two groups, unknown participants, contradictory shaping rules.

## Configuration block in ethereum-package

Added to `network_params`:

```yaml
network_disruption:
  enabled: true
  default_scope: [cl_p2p, el_p2p]   # never touch RPC/engine/metrics by default
  initial_state:
    shaping:
      - name: baseline-jitter
        target: all
        delay: 50ms
  scheduled_events:
    - name: fork-partition
      at_epoch: 5
      action: set
      partitions:
        - groups: [[participant_1, participant_2], [participant_3, participant_4]]
    - name: heal-after-4-epochs
      after: fork-partition
      delay_seconds: 768
      action: clear
```

Anything declarable here is also expressible via the runtime API. Static config is just an early `PUT /v1/state` issued by the launcher.

## Scenarios it can model

- N-way partitions (2, 3, 4+ groups; isolate one node).
- Symmetric N-way splits; asymmetric splits are future work.
- Per-node shaping (delay, jitter, loss, bandwidth); per-edge shaping, dup, and reorder are future work.
- Per-layer scope (CL p2p only, EL p2p only, both).
- Time-scheduled events relative to genesis (epoch/slot/seconds), one-shot or chained.
- Single-node degradation, supernode-vs-leaf asymmetry.
- Mixed disruptions in one run (split + slow node + global jitter).
- Healing as a first-class action.
- Reactive disruption *via* an external watcher posting to the API (assertoor, custom).

## Sharp edges to design in from day one

1. **Conntrack/connection teardown on partition.** Partitions should kill existing TCP sessions, not just drop new packets. Without this, partitions feel soft for tens of seconds and behaviour looks confusing.
2. **Default scope = p2p only.** RPC/engine/metrics/VC↔CL stay reachable unless explicitly opted in. Otherwise assertoor loses visibility into the system it's testing.
3. **Single source of truth for group identity.** Choose either `participant.group:` field on participants OR named selectors (`groups: [[p1, p2], …]`). Don't ship both.
4. **Symmetric by default.** v0 only supports symmetric partitions; reject asymmetric requests before touching kernel state.
5. **Validation up front.** Reject overlapping groups, unknown names, contradictory rules at config/PUT time, with clear errors.
6. **`GET` reflects reality.** Show *applied* state, not last-requested. Lets tests detect controller bugs.
7. **Event log is append-only.** A failing CI run must be able to reconstruct *what the network looked like* at any moment.
8. **discv5 rejoin behaviour after heal.** Expect ENR aging; bootnode-mediated re-discovery is normal. Document, don't try to "fix."

## Build order

1. **disruptoor v0** — standalone repo. Implement tc backend (existing prior art proves feasibility), iptables partition backend, conntrack teardown, HTTP API. Test against a hand-rolled `docker-compose.yml` with two dummy containers. No Kurtosis dependency.
2. **ethereum-package integration** — `additional_services: [disruptoor]`, `network_disruption:` block in `network_params`, name→selector translation, epoch→seconds translation, launcher in `src/disruptoor/`.
3. **assertoor task type** — `run_network_disruption` task posting to the in-enclave endpoint. Last, because by step 2 the system is already drivable from `curl`.

Do steps 1 and 2 in lockstep early on. The API gets validated by real package usage faster than designing it in a vacuum.

## Open questions

- **`iptables` vs `nftables` vs `tc filter … action drop`** for the partition backend. iptables is simplest; nftables is the modern path; staying in tc keeps one mental model. Decide during disruptoor v0. iptables + ipset is probably the right starting point.
- **Where do bridge-side rules vs netns-side rules live?** Bridge-side `FORWARD` rules are fewer and centralised; netns-side rules are isolated per container. Mixed approach is likely.
- **How does disruptoor handle container restarts?** Rules tied to a netns disappear when the container restarts. Either re-apply on docker events, or document that restarts heal that node automatically.
- **Multi-enclave isolation.** If two enclaves run on the same host, disruptoor must scope its actions to its own enclave's containers. Filter by Kurtosis enclave label.
- **Initial-state ordering.** Should `initial_state` apply before or after participants finish syncing the genesis block? Probably after — running the package, then issuing the first PUT once nodes are healthy.

## References (read these before starting)

- `cskiraly/ethereum-kurtosis-tc` — `bin/kurtosis-tc.sh` shows label discovery, port-exclusion filtering, and tc qdisc layering. Good reference for the tc parts.
- `cskiraly/ethereum-fork-testing` — `testfork.sh` and `timing/network-disrupt-at-fork.sh` show the pain we're replacing: ad-hoc bash, manual sudo, no reproducibility.
- ethereum-package `src/spamoor/`, `src/assertoor/`, `src/tx_fuzz/` — examples of how to wire a new additional service.
- ethereum-package `src/package_io/input_parser.star` — where `network_disruption` parsing and validation will land.
- ethereum-package `CLAUDE.md` — house rules for the package.

## Success criteria

A user can write a `network_params.yaml` with `additional_services: [disruptoor]` and a `network_disruption:` block, run `kurtosis run .`, and have the network split at a chosen epoch and heal at another, with assertoor asserting finality recovery — all reproducibly, on their laptop, in Docker, without writing shell scripts.
