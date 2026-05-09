# disruptoor

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

### Use the published image

```bash
docker pull ethpandaops/disruptoor:master-latest
```

The container needs `--privileged` (or `cap_add: NET_ADMIN`) and access to the Docker socket and the host PID namespace to enter container netns'es:

```yaml
services:
  disruptoor:
    image: ethpandaops/disruptoor:master-latest
    privileged: true
    pid: host
    network_mode: host
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command:
      - --addr=:7700
      - --enclave-id=<your-enclave-uuid>
```

## API

Declarative, versioned, idempotent. Full request/response shapes live in `disruptoor.md`.

| Method | Path                | Purpose                                               |
|-------:|---------------------|-------------------------------------------------------|
|  `PUT` | `/v1/state`         | Replace the entire desired disruption state.          |
|  `GET` | `/v1/state`         | Return *applied* state (reflects reality).            |
|  `GET` | `/v1/events`        | Append-only log of every applied change.              |
| `POST` | `/v1/state/clear`   | Heal everything.                                      |
|  `GET` | `/v1/healthz`       | Liveness probe.                                       |

## Building from source

```bash
make build           # produces ./bin/disruptoor
make test            # go test ./...
make test-race       # go test -race ./...
```

Or as a Docker image (multi-arch via buildx):

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t disruptoor:dev .
```

## Goals & non-goals

- **Goals:** Docker-first, declarative, programmable at runtime, reproducible, Ethereum-agnostic core, safe by default (never disrupts RPC/engine/metrics unless explicitly opted-in).
- **Non-goals:** application-layer manipulation (gossip mutation), Byzantine client behaviour, Kubernetes support, clock skew / disk pressure / OOM injection.

See `disruptoor.md` for the full list.

## Contributing

PRs welcome. Run `make test-race` and `gofmt -s -w .` before opening a PR. CI runs `go vet`, `gofmt`, `staticcheck`, and tests with the race detector.

## License

[MIT](./LICENSE)
