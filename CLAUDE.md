# Notes for Claude

## ⛔ HARD RULE — never touch a node's subscriptions[]

When syncing config between nodes (or between node ↔ local), the
`subscriptions:` block in `gateway.yaml` is **off-limits**. Do not add,
remove, rename, reorder, or change URL/UA on any entry.

Each node's subscription list is **operator-owned** and deliberately
different per node (geography, contract terms, cost). Treating it as
"drift to normalize" silently breaks production — caught when 92's
cutover (2026-06-07) overwrote prod subscriptions with 89's set.

If you migrate schema across nodes, splice the target node's
`subscriptions:` block back in unchanged. The safe sync path is
`make redeploy-92` (or `make redeploy-full NODE=dianwei@<ip>` for any
other host) — those pull the on-node yaml first and feed it back verbatim.

This rule overrides any default "make configs consistent" instinct.

## Other useful entry points

- `make help` — list of all targets
- `scripts/redeploy-full.sh <host>` — yaml-first full reinstall
- `scripts/stress.sh` — single-node capacity sweep (run on the node)
- `docs/api.md` — REST API reference
- `configs/gateway.example.yaml` — annotated config template
