# Community node program — ops runbook

How the cl0q team runs the volunteer node program in production.
Operator-facing docs live in `NODE_OPERATORS.md`; the wire protocol in
`PROTOCOL.md`. This is the team's side: monitoring, incidents, and the
levers that keep dozens of volunteer nodes safe to run.

## 1. Health signals (what to watch)

Node-side metrics (`METRICS_ADDR`, `/metrics` and `/status`):

| Signal | Healthy | Investigate |
|---|---|---|
| `node_rows_forwarded_total` rising | steady climb | flat for >2 cycles = stuck forwarder or dead ingest |
| `node_ingest_failures_total` | occasional | sustained climb = ingest trouble, see §3 |
| `node_ingest_rate_limited_total` (A1) | rare | sustained = node over quota or ingest overloaded |
| `node_auth_state` | 0 | 1 = token failing (backoff), 2 = revoked, see §4 |
| `node_watermark_lag_website` / `_domain` | drains each cycle | growing unbounded = backpressure or local DB trouble |
| `node_db_bytes` | stable | growing fast = dead-letter or watermark stall |

The node's `status` subcommand (`cl0q-node status`) renders the same as
an operator snapshot — ask for its output in every support thread.

## 2. The quota model

Each node is allowed **25,000 submissions/day**. The ingest answers
`429` with a `Retry-After` header when a node is over quota (or when
the ingest itself is saturated). Nodes honor `Retry-After` (A1):
the forwarder holds off until it elapses, capped at 1h per cycle, and
no submission is lost — batches stay behind the watermark and retry.

Do **not** raise the per-node quota to fix a slow day. If aggregate
domains/day is below target, the lever is *more nodes* (A1+A2), not
bigger quotas — bigger quotas concentrate abuse risk in fewer hands.

## 3. Ingest incidents

**Ingest 429 storm (many nodes rate-limited at once):** the ingest is
saturated, not the nodes. Scale the ingest or shed load; nodes will
back off on their own and catch up. Do not tell operators to restart —
restarts don't help and the watermark makes them unnecessary.

**Ingest 5xx:** nodes treat 5xx as retryable and keep rows locally.
No operator action needed. If 5xx persists >30 min, treat as an ingest
outage: page per the normal on-call path.

**Node reports "sends nothing":** check in order —
1. `preflight` output (ingest reachability + token validity),
2. `node_auth_state` (2 = revoked, see §4),
3. watermark lag (growing = backpressure, flat at 0 = crawler not producing rows).

## 4. Trust, quarantine, revocation

Trust states: `quarantined` → `trusted` → (`revoked`).

- **Graduation** is automatic (1,000 spot-verified submissions with
  3 clean days, or 1 verified submission with 7 clean days flat).
  There is no manual graduation — don't promise it in support threads.
- **SSRF guard trips** (3 in 24h) reset the clean-day count. An
  unmodified node never trips this; a tripping node is tampered with
  or misconfigured — ask for `status` output and the node version.
- **Revocation** (token rejected): the node's `auth` layer wipes the
  token and re-enrols on the next run *only if the revocation was a
  rotation*. A deliberate revocation stays revoked — the node cannot
  re-enrol itself out of it. Revoke for: confirmed abuse, token leak
  the operator won't rotate, repeated SSRF trips.
- **Re-enrol is authenticated rotation**: it requires the
  `refresh_secret`, never the `node_id` alone. If an operator lost
  `cl0q-node.json` entirely, they re-enrol as a *new* node (new
  quarantine) — there is no recovery path for a lost refresh secret,
  by design.

## 5. Dead letters

Batches the ingest rejects permanently (4xx other than 401/403/408/429)
land in the node's local `cl0q_node_deadletter` table and the node moves
on. `node_rows_deadlettered_total` counts them.

Dead letters are **evidence, not errors**: a spike means the ingest
started rejecting something it used to accept (schema drift, new
validation). Compare the dead-letter `error` text against recent ingest
deploys before blaming the node fleet.

## 6. Onboarding a wave of operators

1. Point them at `NODE_OPERATORS.md` — five minutes, one enroll command.
2. Every new node starts **quarantined**; say so up front so the
   "why aren't my pages in the index" ticket never gets filed.
3. Ask for `--contact` at enrol time. Nodes without a contact are
   unreachable for abuse reports — chase the top submitters monthly.
4. Ask for `--label` too (e.g. `"UK Home Server"`). It shows in the
   admin view and makes support threads unambiguous about which box
   we're talking about.
5. After a wave, watch aggregate `node_rows_forwarded_total` across the
   fleet, not per-node numbers. Measure the program by **net-new
   domains/day** (A1+A2 sequencing note).

### Enrollment confirmation checklist (GAP-3)

The UK-node double-enroll happened because the operator got a bare
node_id and no confirmation the node was actually working. When an
operator says "I enrolled but nothing is happening", walk this list —
do not tell them to re-enrol until step 4 fails:

1. **Read their enroll output back to them.** Since GAP-3 the client
   prints `enrolled as node <id>`, the quarantine status in plain
   language, and the three confirmation steps. If they can't produce
   that output, they didn't complete enrol — have them run it once,
   slowly, and paste the result.
2. **`cl0q-node preflight`** — ingest reachability + token validity.
   A 403 here usually means source-IP mismatch (GAP-1: tokens are bound
   to the enrolment IP) — the fix is re-enrolling from the node's real
   address, not retrying.
3. **`cl0q-node status`** after starting — the operator snapshot.
   `node_rows_forwarded_total` climbing = working.
4. **Only if 1–3 fail**: check `node_auth_state` (2 = revoked, see §4)
   and watermark lag before ever suggesting re-enrol. Re-enrol is
   authenticated rotation, not a retry button — a second blind enrol
   creates a second quarantined node and doubles the confusion.

## 7. What not to do

- Don't merge node PRs that weaken politeness (robots.txt, per-host
  pacing, the 429 backoff) — these are the abuse controls.
- Don't hand out quota exemptions; add nodes instead.
- Don't debug a node from the server side alone — always get
  `cl0q-node status` output first.
