# Changelog

All notable changes to the cl0q crawler node. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [0.1.0] — 2026-10-04

First public node release. A complete, operator-ready crawler node:
enroll in one command, verify with preflight, run with Docker Compose.

### Added
- **Enrol client** (`cl0q-node enroll`): one-command enrolment per
  PROTOCOL.md §1. Stores node_id + token as JSON with mode 0600, never
  prints the token. Re-enrol is idempotent rotation.
- **Preflight self-check** (`cmd/preflight`): six PASS/FAIL checks —
  local DB, ingest reachability + token validity, DNS, HTTPS egress,
  netguard SSRF guard, robots.txt fetch + parse. Exit 1 on any failure.
- **Single-URL dry-run** (`SINGLE_URL=`): crawl one URL through the real
  pipeline and print exactly the JSON the node would send, writing nothing.
- **Startup config validation** (`internal/config`): every knob validated
  at startup; invalid values refuse to start with a clear error naming the
  knob, value, and allowed range. Unparsable input is fatal (no silent
  defaults). Politeness floors are hard minimums. GOMEMLIMIT defaults to 2GiB.
- **Token auth hardening**: 401 triggers automatic re-enrol (when the node
  has a credential file); 403 + `{"revoked": true}` wipes the token and
  tells the operator to re-enrol; exponential backoff (1m→1h) so a dead
  token never spins; token never logged.
- **Bounded footprint** (`internal/footprint`): `NODE_MAX_DOMAINS`,
  `NODE_MAX_PAGES`, `NODE_MAX_DB_GB` caps. At cap, discovery pauses but
  crawling/forwarding continues, with a throttled "at cap" log.
- **Observability** (`internal/metrics`): Prometheus `/metrics` + human
  `/status` per binary via `METRICS_ADDR`. Crawl outcomes, breaker state,
  forward counts, auth state, watermark lag, DB sizes.
- **Operator docs** (`docs/NODE_OPERATORS.md`): 5-minute quickstart,
  stop/update/uninstall, troubleshooting, plain-language data & privacy.
- **Etiquette conformance suite**: property-style tests for throttle
  delays, Crawl-delay obedience, per-group caps, breaker backoff shape.
- **Never-lose-a-domain suite**: row-conservation proof across the
  forwarder (sent XOR dead-lettered XOR pending, exactly once).

### Security
- Netguard SSRF guard refuses private/loopback/link-local/cloud-metadata
  addresses on every outbound connection (verified by preflight).
- Ingest token travels only over HTTPS (or explicit opt-in HTTP for local
  testing); the HTTP client never follows redirects.
- Credential file forced to mode 0600; token never appears in logs, errors,
  or enrol output.
