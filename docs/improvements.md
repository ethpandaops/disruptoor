# Improvements

This document tracks practical testing improvements worth considering for
disruptoor. It is intentionally scoped to fault-injection primitives that are
useful for local Kurtosis devnets; broader scenario orchestration belongs in
assertoor.

## assertoor boundary

Most of the workflow value provided by Chaos Mesh style tooling should live in
assertoor for this stack:

- scenario sequencing
- pre/post health checks
- pass/fail assertions
- artifact collection
- test reporting

disruptoor should stay focused on applying and clearing Docker/Kurtosis network
faults. Non-network faults such as CPU pressure, disk IO faults, process kills,
and clock skew are only worth adding here if they become required test
primitives that assertoor cannot drive elsewhere.

## Useful additions

### Netem packet behavior

Add support for these `tc netem` impairments on shaping rules:

- `reorder`
- `duplicate`
- `corrupt`

Priority:

1. `reorder`: highest value for gossip and network robustness tests.
2. `duplicate`: useful for some transport edge cases, but lower priority.
3. `corrupt`: likely lowest value for Ethereum client testing because bad
   packets are usually dropped quickly by lower layers or authenticated
   protocols, but it is cheap to support if the netem schema is already being
   extended.

These are the clearest useful features to borrow from
`ethereum-kurtosis-tc`.

### Container restart reapply

If a target container restarts, its network namespace loses tc/iptables state.
disruptoor should consider watching Docker container lifecycle events and
reapplying the current desired state when an in-scope container restarts.

This is useful for longer assertoor tests where a client restart should not
silently clear the active disruption.

### P2P-only shaping

Partitions already support p2p-only scope via `cl_p2p` and `el_p2p`, but
shaping currently applies to the whole target `eth0` egress path. The API
therefore requires `scope: ["include_control"]` as an acknowledgement that RPC,
metrics, engine API, and other control traffic are affected too.

P2P-only shaping would allow delay/loss/reorder/duplicate/corrupt to affect
gossip and discovery traffic while keeping control-plane visibility intact.
This is useful, but should be treated as a separate tc-filtering improvement
rather than part of the basic netem packet-behavior work.

## Not prioritized

The following features from `ethereum-kurtosis-tc` do not look important for
our current local-devnet testing use case:

- Separate upload and download impairment.
- Host-side ingress/downlink shaping.
- One-shot shell CLI behavior.
- Query-param HTTP endpoints.
- EL/CL command-line flags, unless they become selector presets in the API or
  web UI.

For local devnets, symmetric in-enclave network faults are usually enough, and
the existing state API is a better integration point for assertoor than a shell
wrapper.
