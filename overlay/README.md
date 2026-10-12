# Private overlay — skeleton (Track O, O2 PoC)

The canonical repo (this one) holds **zero secrets**. Everything prod-only —
database credentials, API tokens, signing keys, operator contact details —
lives in the **private overlay**, a separate private repo that is never
published and never becomes public at the 100M flip.

## The boundary

| Lives in canonical repo (this) | Lives in private overlay |
|---|---|
| All Go code, incl. config *parsing* | Real config *values* (DSN, tokens, keys) |
| `docker-compose.yml` with placeholder env | `docker-compose.override.yml` with `env_file` |
| `overlay/env.example` (this dir — fake values only) | `overlay/.env` (real values, git-ignored, never committed) |
| Docs, migrations, build system | Prod deploy glue, systemd units, host firewall |

Rules:
1. **No secret may enter the canonical repo.** The O4 scrub-guard CI job
   will grep for DSN hosts, `user:password@`, and honeypot symbols.
2. **Fail fast.** Every secret comes from the environment; the overlay's
   `load-env.sh` refuses to start a service when a required variable is
   unset or still holds a placeholder. A typo'd env var must never silently
   downgrade to an insecure default.
3. **Prod builds from canonical.** The overlay never contains Go code that
   changes node behavior — only config, secrets injection, and deploy glue.
   `make build` runs against a pinned canonical commit; the overlay only
   wraps the resulting binaries.

## Using this skeleton

```sh
cp overlay/env.example overlay/.env      # then fill in REAL values
cp overlay/docker-compose.override.example.yml overlay/docker-compose.override.yml
source overlay/load-env.sh              # fails fast on missing/placeholder values
docker compose up -d
```

`overlay/.env` is git-ignored by the overlay repo's own `.gitignore`
(see `overlay/gitignore.example`). Never copy a real `.env` into the
canonical repo, never paste one into chat, docs, or issues.
