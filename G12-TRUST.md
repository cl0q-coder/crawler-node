# G-12: Node trust + result verification — design & runbook

Volunteer nodes are untrusted by definition. Google doesn't have this
problem (every machine is theirs). We do. The rule is **never trust,
always verify** — and make lying expensive.

## Architecture

```
                    ┌─────────────────────┐
  node crawls page  │   internal/trust    │
        │           │                     │
        ▼           │  integrity.SignResult│  HMAC-SHA256(nodeID, URL, hash, ts)
  signed submission ├─▶  verifier pipeline│  sample → trusted re-fetch → compare
        │           │  TrustScorer         │  EWMA(verif .6, uptime .25, quality .15)
        ▼           │  sybil.PoW          │  Hashcash at enrollment
  ingest / index    └─────────────────────┘
```

### 1. Result verification (implemented: `internal/trust/verifier.go`)

The trusted verifier samples a fraction (`SampleRate`, server-configured)
of each node's submissions and re-fetches the URL from infrastructure the
node cannot influence. The SHA-256 of the trusted fetch is compared to the
node's claimed hash. Three checks, in order:

1. **Staleness** — the submission's `claimed_at` must be within `MaxAge` of
   now. The integrity signature binds a timestamp, so this is also the
   replay guard.
2. **Signature** — `VerifyResult` with the node's secret (its
   `refresh_secret`, which the server already holds from enrollment). A
   failed signature means the submission was tampered with in transit or
   misattributed to another node.
3. **Content match** — trusted re-fetch hash vs claimed hash. This is the
   check a signature alone cannot provide: a dishonest node can sign its
   own lie perfectly.

Every outcome is recorded in the scorer. A fetch failure is recorded as a
non-match (fail-closed): a node whose pages systematically "can't be
re-fetched" looks exactly like a node hiding poisoned content.

**Sampling rate guidance:** 1–5% per node is enough to catch systematic
dishonesty quickly (probability of missing N consecutive lies is
(1−p)^N; at p=0.02, 200 lies → 98% detection) while keeping verifier
bandwidth bounded.

### 2. Trust scoring (implemented: `internal/trust/score.go`)

Per-node EWMA score from three signals:

| Signal | Weight | Source |
|---|---|---|
| Verification outcomes | 0.60 | verifier (the only signal an adversary can't fake cheaply) |
| Heartbeat uptime | 0.25 | node heartbeats (HE-1 §) |
| Data quality | 0.15 | fraction of submitted rows corrupt/unparseable/rejected |

Tiers and enforcement (`RouteWork`):

| Tier | Score | Work received |
|---|---|---|
| `full` | ≥ 0.80 | standard queue |
| `restricted` | ≥ 0.50 | low-value work only (probe reserve, low-priority recrawls) |
| `quarantined` | < 0.50 | none — held for verifier re-promotion |

New nodes start at 0.60 **provisional** and can never reach `full` before
25 verification outcomes, no matter the score — trust is earned, not
assumed. An operator `quarantineHold` pins a node regardless of score and
is only cleared by the re-promotion flow. Single failures never quarantine
a node (fail-soft against verifier flakiness); sustained dishonesty does.

### 3. Sybil resistance (implemented: `internal/trust/sybil.go` + enroll wiring)

Enrollment carries a Hashcash-style proof-of-work: the server issues
`{seed, difficulty}`; the client finds a nonce with
`SHA256(seed‖nodeID‖nonce)` having ≥ difficulty leading zero bits, then
POSTs it to `/api/nodes/enroll/confirm`. Verification is one hash.

- Client side is **backward compatible**: no challenge in the enroll
  response → no PoW. (`cmd/cl0q-node/enroll.go`: `confirmPoW`.)
- Server side needed (Claude): issue challenges on enroll, verify with
  `VerifySolution`, activate the node only after confirm.

**Difficulty guidance:** 16–20 bits ≈ seconds of laptop CPU (casual Sybil
deterrent); 22–24 bits ≈ minutes (serious onboarding friction). The server
chooses per fleet conditions; a Sybil wave is answered by raising it.

#### Why PoW and not the alternatives

| Mechanism | Cost to attacker for N fake nodes | Cost to honest operator | Verdict |
|---|---|---|---|
| Proof-of-work | O(N · 2^difficulty) compute, every node | seconds–minutes once per enroll | **Chosen.** No identity infrastructure, no money, verifiable by one hash. |
| Invite tiers | social — must compromise an inviter | friction finding a sponsor | Good second layer for *trusted* tier, not for open enrollment. |
| Stake / bond | O(N · stake) capital | excludes genuine volunteers | Wrong for a volunteer fleet; consider for paid tiers later. |
| CAPTCHA | O(N · solver-API cost) | annoying, automatable anyway | Weakest; solver APIs made it a commodity. |

Recommended posture: PoW at open enrollment (this PR) + invite tiers for
promotion to higher-trust work classes later. Revisit stake if a paid
operator tier ever exists.

### 4. Data integrity (implemented: `internal/trust/integrity.go`)

`SignResult(secret, nodeID, url, contentHash, ts)` → HMAC-SHA256 over a
canonical message with 0x00-separated fields (no concatenation ambiguity).
The secret is the node's `refresh_secret`, already shared at enrollment.
Downstream systems (ingest, index builders, anyone holding the secret)
detect any tampering with node identity, URL, content, or timestamp.

## What the server still needs (for Claude)

1. Issue `pow_challenge {seed, difficulty}` in the enroll response;
   verify `POST /api/nodes/enroll/confirm {node_id, pow_nonce}` with
   `VerifySolution` before activating the node.
2. Run the verifier worker loop: sample submissions → trusted re-fetch →
   `Verifier.MaybeVerify` → feed `TrustScorer` → enforce `RouteWork` tiers
   in the work-assignment path.
3. Admin quarantine view + re-promotion flow (overlaps HE-1's admin gap).

## Demonstration

`internal/trust/trust_test.go` — `TestDishonestNodeDetection` runs an
honest node and a dishonest node (valid signature, tampered content —
the case signature-only checking misses) through the verifier:

- honest node: 60/60 matches → climbs provisional → `TierFull`.
- dishonest node: 60/60 mismatches → detected → `TierQuarantined`,
  `RouteWork` returns `"none"`.

Also covered: provisional cap, stale-replay refusal, quality/uptime
signals, operator quarantine hold, PoW solve/verify round-trips.

## Operator runbook

- **New node enrolls:** provisional at 0.60, `restricted` at best. After
  25 clean verifications it becomes eligible for `full`.
- **Node drops to `restricted`:** check verifier outcomes — usually a
  burst of mismatches (bad fetch path on the node) or deadletter spikes.
  It recovers automatically as outcomes improve (EWMA).
- **Node quarantined:** inspect recent `Outcome.Detail` strings. If the
  node was compromised, rotate: re-enroll (new `refresh_secret` rotates
  the signing key). If it was verifier-side flakiness, use the
  re-promotion flow (`ClearQuarantineHold` + a verifier sweep).
- **Sybil wave:** raise enroll PoW difficulty; watch the enroll confirm
  rate — legit operators solve in seconds, bot farms stall.
- **Key rotation:** the signing secret IS the `refresh_secret`; rotation
  re-keys signatures automatically. Old-signatured submissions fail
  verification during the overlap — keep `MaxAge` wider than the rotation
  window.
