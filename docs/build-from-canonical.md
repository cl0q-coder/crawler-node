# Build from canonical — the O2 model (Track O)

One page on how cl0q kills "two hand-synced trees" drift.

## The problem

Today prod runs binaries built from a tree that is hand-synced with the
public repo. "Public == prod" is a promise kept by discipline, which rots:
a hotfix lands in prod but not in the repo, or vice versa, and nobody can
prove which source a running binary came from.

## The model

1. **The canonical repo is the single source of truth.** All node code
   lives here. It contains **zero secrets** — DSN, tokens, keys all come
   from the environment (see `overlay/`).
2. **Prod is built from a pinned commit.** `make build` refuses a dirty
   tree, compiles every binary with `-trimpath`, and stamps each one with
   the exact commit SHA and UTC build time via ldflags
   (`internal/version`: `Version`, `Commit`, `BuildTime`).
3. **The stamp is verifiable at runtime.** Every binary answers
   `--version` with `version=<VERSION> commit=<SHA> built=<time>` — before
   any config validation, so it works on an unconfigured box. The O3
   `cl0q-provenance` verify tool will query each running service and
   compare the reported commit against the shipped tag; GREEN means
   prod == tag, RED means drift.
4. **Secrets and prod glue live in the private overlay.** A separate
   private repo holds `.env`, the compose override, systemd units, and
   firewall rules. The overlay never contains behavior-changing code — it
   wraps the pinned canonical build. At 100M the canonical repo flips
   public; the overlay stays private forever.

## Day-to-day

```sh
git checkout <pinned-commit>   # or a release tag
make build                     # refuses dirty tree; stamps commit SHA
./dist/cl0q-node --version     # cl0q-node version=0.1.1 commit=abc1234 built=2026-10-11T...
```

For local iteration without committing: `make dev-build` (stamp reads
`dev-<sha>` so it can never be mistaken for a canonical build).

## What this PoC does and doesn't do

Does: stamping plumbing, `--version` on all six binaries, dirty-tree
refusal, the overlay skeleton + fail-fast env loader, this doc.
Doesn't (later O-track phases): the `/version` HTTP endpoint + the
`cl0q-provenance` verify tool (O3), the CI scrub-guard blocking secrets
from entering the repo (O4a), reproducible byte-identical builds (O5).
