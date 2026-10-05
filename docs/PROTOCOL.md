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
  "operator_contact": "operator@example.com"
}
```

`operator_contact` is optional but recommended — it is how cl0q reaches the
operator about abuse reports or revocation.

Response `200 OK` (JSON):

```json
{
  "node_id": "nd_7f3a9c",
  "token": "<secret, shown once>",
  "ingest_url": "https://cl0q.com",
  "quarantined": true
}
```

- The node stores the token with mode `0600` in its config dir and **never
  logs it** (not even at debug level).
- Every new node starts `quarantined: true` (see §3).
- **Re-enrol / rotation** is idempotent: `POST /api/nodes/enroll` with
  `{"node_id": "nd_7f3a9c"}` returns a fresh token for the same node id.
  The old token stops working; in-flight batches are retried with the new
  one, nothing is lost.

## 2. Authentication

Every ingest call carries:

```
Authorization: Bearer <token>
```

- The HTTP client **never follows redirects** — a redirect response is
  surfaced as an error, so the token only ever goes to `ingest_url`.
- The server checks the token (it stores only the SHA-256 hash) and the
  source IP the node enrolled with. Note: nodes behind rotating egress IPs
  will trip the IP check — re-enrol, or contact cl0q; the quarantine in §3
  is the primary trust mechanism, the IP check is a secondary signal.

## 3. Trust states

| State | Meaning |
|---|---|
| `quarantined` | New node. Submissions land in a staging area and do **not** reach the live index. cl0q spot-verifies a sample: re-fetches the URL, compares `content_sha256`, confirms `robots_rule` and netguard were honored. |
| `trusted` | Graduated. Submissions flow to the live index normally. |
| `revoked` | Token rejected. The node wipes its token and re-enrols on the next run. |

**Graduation** (server-side, both conditions or the flat rule):

- 1,000 spot-verified samples at 98%+ hash match **and** 3 clean days, **or**
- 7 clean days flat.

**Quotas**: 10k submissions/day per node while quarantined (above a real
node's early pace, low enough to bound junk). **Strike rule**: 3 failed
verifications in a rolling 24h window re-quarantines the node.

## 4. Submission envelope

`POST /api/ingest/website` and `POST /api/ingest/domains`.
Body: `{"rows": [...]}` — at most 2000 rows per request, `Content-Type:
application/json`, `User-Agent: Mozilla/5.0 (compatible; cl0q-node/1.0;
operator=<node-id>; +https://cl0q.com/bot)`.

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
| `401` | Token invalid/expired: re-enrol automatically (idempotent), exponential backoff. Spool submissions to the local SQLite WAL meanwhile — **nothing is lost, nothing blocks silently**. |
| `403` + `{"revoked": true}` | Wipe the token, re-enrol on the next run. Never keep submitting. |
| `429` | Back off per `Retry-After`; quota exceeded → spool, don't drop. |
| `5xx` | Retry with backoff; after N attempts the batch goes to the local dead-letter table (existing `cl0q_node_deadletter`) and the node carries on. |

## 6. Versioning

- The `version` string in the enrol request lets the server reject ancient
  nodes with `426 Upgrade Required` instead of silently mis-parsing them.
- Envelope fields are **additive only**: the server ignores unknown fields,
  the node tolerates missing new fields from older peers. No breaking
  changes without a protocol version bump (this document's `v1`).
