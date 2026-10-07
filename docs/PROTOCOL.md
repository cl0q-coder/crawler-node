# PROTOCOL.md — cl0q node wire protocol (v1)

The contract between a crawler node and cl0q's ingest API. Trust model: **A —
open enrol + server-side quarantine** (see the audit doc 2026-10-05 for the
decision). Anyone can enrol with one command; nothing a node submits reaches
the live index until cl0q has spot-verified it.

This document is the lock point: the node implements the client side,
cl0q-ingest implements the server side to match. Changes here need both
sides to agree first.

## 1. Enrol — `POST /api/nodes/enroll`

Open enrol, no prior auth (model A). One command on the node:

```
docker compose run --rm cl0q-node enroll
```

Request (JSON):

```json
{
  "version": "cl0q-node/1.0",
  "operator_contact": "operator@example.com",
  "operator_label": "UK Home Server"
}
```

`operator_contact` is optional but recommended — it is how cl0q reaches the
operator about abuse reports or revocation. `operator_label` is optional — a
human-friendly name for the node, shown in the admin view and echoed back in
the response.

The server records the enrolling source IP (from `X-Real-IP`, which the
fronting proxy sets) into the node row alongside the contact and label.
Tokens are bound to the enrolment network: a request from a different
source IP gets `403` with a revocation flag (see §2) — the operator
re-enrols from the node's real address.

Response `200 OK` (JSON):

```json
{
  "node_id": "nd_7f3a9c",
  "token": "<secret, shown once>",
  "refresh_secret": "<secret, shown once>",
  "ingest_url": "https://cl0q.com",
  "quarantined": true,
  "operator_label": "UK Home Server"
}
```

- The node stores `node_id`, `token`, and `refresh_secret` in its config file
  with mode `0600` and **never logs them** (not even at debug level). Both
  secrets appear once, in this response only — never printed to the terminal.
- Every new node starts `quarantined: true` (see §3).
- **Rotation** (authenticated — not an idempotent re-enrol): to mint a fresh
  token, `POST /api/nodes/enroll` with
  `{"node_id": "nd_7f3a9c", "refresh_secret": "<stored secret>"}`. The server
  compares the refresh_secret (constant-time) and returns a new token; the old
  token stops working, in-flight batches retry with the new one, nothing is
  lost. A request carrying **only** the `node_id` is refused: the node_id is
  public — it appears in the crawl User-Agent — so it can never, by itself,
  rotate credentials. The `refresh_secret` is never sent on ingest calls and
  itself never rotates; keep it as secret as the token. A node running on a
  bare `NODE_TOKEN` env var has no stored refresh_secret and so cannot rotate —
  enrol into a credentials file instead.

## 2. Authentication

Every ingest call carries:

```
Authorization: Bearer <token>
```

- The HTTP client **never follows redirects** — a redirect response is
  surfaced as an error, so the token only ever goes to `ingest_url`.
- The server checks the token (it stores only the SHA-256 hash). Open-enrolled
  nodes are **token-only**: they authenticate from any source IP, so a dynamic
  or rotating egress IP is fine — it does not trip anything. (cl0q can
  optionally pin a specific node to a source-IP CIDR; such a node receives
  `403` from an off-allowlist address. This is not the default and is never set
  by open enrol.) Quarantine (§3) is the trust mechanism, not the source IP.

## 3. Trust states

| State | Meaning |
|---|---|
| `quarantined` | New node. Submissions land in a staging area and do **not** reach the live index. cl0q spot-verifies a sample: re-fetches the URL, compares `content_sha256`, confirms `robots_rule` and netguard were honored. |
| `trusted` | Graduated. Submissions flow to the live index normally. |
| `revoked` | Token rejected. The node wipes its token and re-enrols on the next run. |

**Graduation** (server-side; the count rule or the flat rule):

- **1,000** spot-verified samples — fetches whose `content_sha256` the server
  re-fetched and matched — **and** 3 clean days, **or**
- at least one verified sample **and** 7 clean days flat.

There is no percentage gate: a sampled submission either matches (and counts
toward the 1,000) or is dropped, and a drop never advances graduation. A node
that submits junk therefore simply never graduates — fail-closed. A "clean day"
is counted from the node's last strike (or from enrolment if it has none).

**Quotas**: 25,000 submissions/day per node while quarantined (above a real
node's early pace, low enough to bound junk); over quota the server answers
`429` and the node spools (§5) — nothing is dropped. **Strike rule**: a
spot-verification that finds the node contacted a non-public / SSRF address is
a *strike*; 3 strikes in a rolling 24h window reset the clean-day clock (the
node stays quarantined and rebuilds its clean days). A content-hash mismatch is
dropped but is **not** a strike.

## 4. Submission envelope

`POST /api/ingest/website` and `POST /api/ingest/domains`.
Body: `{"rows": [...]}` — at most 2000 rows per request, `Content-Type:
application/json`, `User-Agent: Mozilla/5.0 (compatible; cl0q-node/1.0;
operator=<node-id-prefix>; +https://cl0q.com/bot)`.

`<node-id-prefix>` is the **first 8 characters** of the node_id. The full
node_id never appears in a public User-Agent: combined with the
refresh_secret requirement in §1, a harvested prefix is useless for
rotation, while site operators can still identify the node family in
their logs and block it with one robots.txt group (`User-agent:
cl0q-node`).

### website_info row

Existing fields (unchanged): `domain`, `title`, `description`, `keywords`,
`canonical`, `h1`, `status_code`, `content_type`, `content_length`,
`latency_ms`, `redirects`, `outbound_links`, `image_count`, `word_count`,
`charset`, `robots`, `og_title`, `og_image`, `twitter_card`,
`page_language`, `server_header`, `fetch_error`, `etag`, `last_modified`.

New **audit-evidence** fields (★ — the server uses these for spot
verification without redoing the crawl):

| Field | Type | Meaning |
|---|---|---|
| `content_sha256` ★ | string (hex) | SHA-256 of the fetched response body |
| `fetched_at` ★ | int (unix) | When the fetch happened |
| `final_url` ★ | string | URL after following redirects |
| `tls_cert_sha256` ★ | string (hex) | SHA-256 of the leaf TLS cert (https only; empty for http) |
| `robots_rule` ★ | string | The robots.txt rule that allowed the fetch (e.g. `"allow: /"` or the matched `User-agent` group + rule) |

Page body text is **never** stored or sent — only its hash.

### domain_info row

`domain`, `ipv4`, `ipv6`, `mx`, `cname`, `found_on` (unchanged — DNS
answers are inherently re-verifiable, no extra evidence needed).

### Not sent

Bot-challenge interstitials (content discarded at fetch time) and names
resolving only to non-public addresses (never contacted) are not sent —
same as today.

## 5. Error handling (node side)

| Server response | Node behavior |
|---|---|
| `401` | Token invalid/expired: **rotate automatically** (node_id + refresh_secret, §1) when the node enrolled via `cl0q-node enroll`; a node on a bare `NODE_TOKEN` env var has no refresh_secret, so it stops and alerts rather than silently failing. Spool submissions to the local SQLite WAL meanwhile — **nothing is lost, nothing blocks silently**. |
| `403` + `{"revoked": true}` | Wipe the token, re-enrol on the next run. Never keep submitting. |
| `429` | Back off per `Retry-After`; quota exceeded → spool, don't drop. |
| `5xx` | Retry with backoff; after N attempts the batch goes to the local dead-letter table (existing `cl0q_node_deadletter`) and the node carries on. |

## 6. Versioning

- The `version` string in the enrol request is recorded and reserved so a
  future server can reject incompatible nodes (e.g. `426 Upgrade Required`)
  instead of silently mis-parsing them. v1 servers accept any version.
- Envelope fields are **additive only**: the server ignores unknown fields,
  the node tolerates missing new fields from older peers. No breaking
  changes without a protocol version bump (this document's `v1`).
