<p align="center">
  <img src="./docs/assets/disruptoor-logo.png" alt="disruptoor" width="180" />
</p>

<h1 align="center">disruptoor</h1>

[![Build master](https://github.com/ethpandaops/disruptoor/actions/workflows/build-master.yml/badge.svg)](https://github.com/ethpandaops/disruptoor/actions/workflows/build-master.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ethpandaops/disruptoor)](https://goreportcard.com/report/github.com/ethpandaops/disruptoor)
[![License](https://img.shields.io/github/license/ethpandaops/disruptoor)](LICENSE)
[![Docker](https://img.shields.io/docker/pulls/ethpandaops/disruptoor)](https://hub.docker.com/r/ethpandaops/disruptoor)

A small, privileged Docker sidecar that applies network disruptions — partitions, latency, packet loss, bandwidth caps — to other containers in a [Kurtosis](https://github.com/kurtosis-tech/kurtosis) enclave, controllable via an HTTP API.

Designed to be launched as an `additional_service` of the [ethereum-package](https://github.com/ethpandaops/ethereum-package) and driven from [assertoor](https://github.com/ethpandaops/assertoor) test scenarios, but the core is Ethereum-agnostic and works against any Docker enclave.

> See [`disruptoor.md`](./disruptoor.md) for the full design document.

## Quick start

### Run the smoke test

The repo ships with a `docker-compose.test.yml` that boots two dummy Kurtosis-labelled containers plus disruptoor itself:

```bash
docker compose -f docker-compose.test.yml up --build
```

In another terminal:

```bash
# Liveness
curl http://localhost:7700/v1/healthz

# Inspect what disruptoor sees in the enclave
curl http://localhost:7700/v1/state
```

Trigger a partition that splits the two test containers (`alpha` and `bravo`) on their p2p ports:

```bash
curl -X PUT http://localhost:7700/v1/state \
  -H 'Content-Type: application/json' \
  -d '{
    "partitions": [
      {
        "name": "alpha-vs-bravo",
        "groups": [
          {"id": "alpha"},
          {"id": "bravo"}
        ]
      }
    ]
  }'
```

Verify it bit (alpha can no longer reach bravo's discovery port):

```bash
docker exec disruptoor-test-alpha nc -vz 172.30.0.11 30303
```

Or isolate a single container from **everything else** without enumerating the
counterparties — the complement of `target` is computed at apply time, so the
same request keeps working when the enclave topology changes:

```bash
curl -X PUT http://localhost:7700/v1/state \
  -H 'Content-Type: application/json' \
  -d '{
    "isolations": [
      {
        "name": "blackout-alpha",
        "target": {"id": "alpha"},
        "scope": ["cl_p2p", "el_p2p", "include_control"]
      }
    ]
  }'
```

An isolation is semantically a symmetric two-group partition of `target` vs
the rest of the enclave. `scope` defaults to `[cl_p2p, el_p2p]`; add
`include_control` to also cut RPC/engine/metrics/VC↔CL traffic (a full
blackout). The target must not match every container — there'd be nothing
left to isolate from.

A target matching multiple containers is isolated **as a group**: traffic
among its members keeps flowing (useful for "island" scenarios, e.g. cutting
all beacon nodes off from their EL/VC stacks while they still gossip with
each other). To black out several containers individually, declare one
isolation per container.

Heal everything:

```bash
curl -X POST http://localhost:7700/v1/state/clear
```

### Initial state from a config file

`--config <path>` loads desired state from a YAML or JSON file at startup and applies it before the HTTP API begins serving. Format is autodetected from the extension (`.yaml`, `.yml`, `.json`). Validation errors abort startup with a non-zero exit; nothing is left half-applied.

```bash
disruptoor --config /etc/disruptoor/init.yaml
```

The wire format is identical to `PUT /v1/state` — see [`examples/disruption.yaml`](./examples/disruption.yaml) for a worked example, and [`schemas/v1-state.json`](./schemas/v1-state.json) for the JSON Schema (suitable for parse-time validation in upstream callers like the ethereum-package).

### Use the published image

```bash
docker pull ethpandaops/disruptoor:latest
```

`latest` tracks the most recent published release. For reproducible deployments,
pin an explicit release tag such as `ethpandaops/disruptoor:vX.Y.Z`.

The container needs `--privileged` (or `cap_add: NET_ADMIN`) and access to the Docker socket and the host PID namespace to enter container netns'es:

```yaml
services:
  disruptoor:
    image: ethpandaops/disruptoor:vX.Y.Z
    privileged: true
    pid: host
    network_mode: host
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command:
      - --addr=:7700
      - --enclave-id=<your-enclave-uuid>
```

## Web UI

When disruptoor is running, point a browser at the same `--addr` to access an
embedded control panel (Bootstrap, no external CDN). The UI shares the listener
with the v1 API and lets you:

- See the current applied state at a glance
- Add or remove partitions and shaping rules without crafting JSON by hand
- List the Kurtosis containers the discovery service can resolve
- Browse an in-memory event log of every applied / cleared / failed apply
- Edit and PUT the raw state as JSON

| Path           | Purpose                                       |
|----------------|-----------------------------------------------|
| `/`            | Dashboard summary + recent events             |
| `/partitions`  | Active partitions, plus an Add modal          |
| `/shaping`     | Active shaping rules, plus an Add modal       |
| `/containers`  | All containers visible in the enclave         |
| `/events`      | Mutation log (ring buffer, clears on restart) |
| `/state`       | Raw JSON editor — same wire format as the API |

Pass `--disable-webui` to skip mounting the UI altogether, or
`--webui-event-buffer N` to size the in-memory event ring (default 200).

## API

Declarative, versioned, idempotent. The canonical request/response schema lives
in `schemas/v1-state.json`; broader design notes live in `disruptoor.md`.

| Method | Path                | Purpose                                               |
|-------:|---------------------|-------------------------------------------------------|
|  `PUT` | `/v1/state`         | Replace the entire desired disruption state.          |
|  `GET` | `/v1/state`         | Return *applied* state (reflects reality).            |
| `POST` | `/v1/state/clear`   | Heal everything.                                      |
|  `GET` | `/v1/healthz`       | Liveness probe.                                       |

`GET /v1/state` returns an `ETag`. Send it back as `If-Match` on `PUT /v1/state`
when you want stale writes rejected with `412 Precondition Failed`.

## Building from source

```bash
make build           # produces ./bin/disruptoor
make test            # go test ./...
make test-race       # go test -race ./...
```

Or as a Docker image (multi-arch via buildx):

```bash
docker buildx build --platform linux/amd64,linux/arm64 --build-arg RELEASE=dev -t disruptoor:dev .
```

## Contributing

PRs welcome. Run `make test-race` and `gofmt -s -w .` before opening a PR. CI runs `go vet`, `gofmt`, `staticcheck`, and tests with the race detector.

## License

[MIT](./LICENSE)
