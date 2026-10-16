# Running a cl0q crawler node

A node crawls the public web from your server and sends what it finds to
cl0q's search index. Five minutes from zero to crawling.

## What you need

- A server (VPS, homelab box, anything with Docker) with a **public IP**
  and a provider that allows crawling.
- Docker + Docker Compose.
- About 2 GB RAM free and 10 GB disk for the node's local database.

Nothing listens on a public port. Your node only makes outbound requests.

## 1. Enroll (one command)

```sh
git clone https://gitea.cl0q.com/chris/crawler-node && cd crawler-node
docker compose build
docker compose run --rm --entrypoint /usr/local/bin/cl0q-node node enroll --contact you@example.com --label "My Home Server"
```

This calls cl0q's ingest server and enrolls you as a node. The server
returns three things: your unique `NODE_ID`, a secret token, and a
**refresh secret** (used only to rotate your token later). All three are
stored in `cl0q-node.json` (mode `0600`, never shown or logged — the
secrets are printed nowhere, not even in your own terminal).
The contact email is optional but recommended — it's how cl0q reaches you
if your node ever causes trouble. The `--label` is a friendly name for
your node (shown in the admin view); pick something you'll recognize.

When it finishes, the command tells you plainly: you're enrolled, whether
you're quarantined or trusted, and exactly how to confirm the node is
crawling (`preflight`, then `status`, then watch your forwarded-row count
climb). **Save that output.** If anything looks wrong later, support will
ask for it before anything else — don't re-run enroll blindly; a second
enroll creates a second quarantined node.

### Quarantine, plainly

While quarantined, your node works exactly like a trusted one — except
cl0q **spot-verifies** a sample of your submissions before any of them
enter the live index. Spot-verification means: cl0q re-fetches the URL
itself, compares the page's content hash with yours, and confirms you
honored robots.txt and the address guard. Your submissions are kept in a
staging area meanwhile — **nothing you submit is lost while quarantined**.
A well-behaved node graduates automatically: either 1,000 spot-verified
submissions (fetches cl0q re-fetched and whose content hash matched yours)
with 3 clean days, or at least one verified submission with 7 clean days
flat. There's no percentage to hit — a sampled page either matches or is
dropped, and only matches count toward the 1,000.

Three trust states exist: `quarantined` (new / under verification),
`trusted` (graduated — submissions flow to the live index normally), and
`revoked` (token rejected; the node wipes its token and re-enrols on the
next run).

While quarantined you may send up to 25,000 submissions per day. If you
hit that quota the server answers 429 — your node backs off and spools
the batch to its local database. It retries later. **Submissions are
never dropped.** If cl0q catches your node fetching a non-public or
blocked (SSRF) address 3 times in a 24-hour window, your clean-day count
resets to zero — you stay quarantined and start rebuilding. (An unmodified
node never does this; it's a guard against a tampered one. A page whose
hash simply doesn't match is dropped, not counted against you.)

### Rotation

If a token leaks (or the server revokes it), rotate with the refresh
secret: re-run `enroll`. The client sends your `node_id` **plus** the
stored `refresh_secret`; the server refuses rotation requests that carry
only the `node_id` (it's printed in your public User-Agent, so anyone
could read it off a site's logs). Nodes running on plain `NODE_TOKEN` /
`CL0Q_NODE_ID` environment variables can't rotate — there's no refresh
secret stored there, so enrol into a credentials file first.


## 2. Configure

```sh
cp .env.example .env
```

Edit `.env`: set `DB_PASSWORD` (any strong password — it's only for the
node's own Postgres, unreachable from outside). If you enrolled with the
command above, you don't need to set `NODE_TOKEN` or `CL0Q_NODE_ID` — the
node reads them from `cl0q-node.json`.

### Disk safety

A node with a full disk stops dead. Set `NODE_MAX_DB_GB` in `.env` to
**below your disk size** (e.g. `10` on a 25 GB droplet) — the 20 GB default
cannot save a smaller disk. When the cap is hit the node just pauses
discovery of new domains while it keeps crawling and forwarding what it
has, and logs a clear "at cap" message. Small droplets work fine: 1 GB RAM
is enough as long as the box has 2 GB+ of swap for the initial build.

## 3. Preflight

```sh
docker compose run --rm preflight
```

Six checks, each a clear PASS/FAIL: local database, ingest reachability +
token validity, DNS, outbound HTTPS, the SSRF guard (it must refuse
private addresses), and robots.txt fetching. All six must pass. If the
ingest check fails, your token isn't accepted — re-run `enroll` to rotate
it. (Your node authenticates by token alone; a changed or dynamic public
IP is fine.)

## 4. Go live

```sh
docker compose up -d
docker compose logs -f website-crawler node
```

You should see pages being fetched within a minute. The `node` service
forwards new results to cl0q every 60 seconds.

## 5. Verify it's crawling

```sh
docker compose logs --tail=50 website-crawler | grep -i "crawled\|fetched"
```

Or dry-run a single URL through the real pipeline (writes nothing):

```sh
docker compose run --rm -e SINGLE_URL=https://example.com website-crawler
```

This prints exactly the JSON row the node would send for that URL.

## Stop / update / uninstall

```sh
docker compose down            # stop everything (data kept in the pgdata volume)
docker compose pull && docker compose up -d --build   # update to the latest image
docker compose down -v         # stop AND delete all local data (fresh start)
```

To leave the network for good: `docker compose down -v`, then tell
cl0q (cl0qsearch@gmail.com) so your node id is retired.

## If something breaks

| Symptom | Likely cause | Fix |
|---|---|---|
| `preflight` ingest FAIL | Token rejected (expired/revoked) | `docker compose run --rm --entrypoint /usr/local/bin/cl0q-node node enroll` (rotates the token) |
| `CL0Q_NODE_ID is not set` | Enrol didn't run | Run step 1; check `cl0q-node.json` exists |
| `invalid WORKERS=...` etc. | Bad setting in `.env` | The message names the knob and the allowed range — fix the value |
| Node runs but sends nothing | DB or ingest unreachable | `docker compose logs node`, then `preflight` |

Every setting is validated at startup: a bad value refuses to start with
a clear error instead of silently misbehaving.

---

## Data & privacy

Plain language, because you're trusting this software on your server.

**What your node sends to cl0q**, per crawled page: the domain, page
title, description, keywords, canonical URL, first heading, HTTP status,
content type and length, response time, redirect count, counts of outbound
links/images/words, charset, robots meta tag, Open Graph / Twitter Card
fields, page language, `Server` header, fetch error if any, and ETag /
Last-Modified validators. Per discovered domain: its IP addresses, MX
and CNAME records, and which page it was found on. Plus a SHA-256 hash
of the page content, so cl0q can spot-verify submissions.

**What it never sends:** page body text (only its hash), anything about
you or your server beyond the node id, or any other traffic on your
machine. The node holds no cl0q credentials and cl0q holds none of yours.

**Who else sees part of the work** (as with any crawler): public DNS
resolvers (Cloudflare, Google, Quad9) see the domain names looked up;
ipinfo.io sees IP addresses *only* if you set `IPINFO_TOKEN`.

**What site owners see:** every request carries
`Mozilla/5.0 (compatible; cl0q-node/1.0; operator=<your-node-id>;
+https://cl0q.com/bot)`. A site can block all nodes with one robots.txt
group (`User-agent: cl0q-node`), and the node always honors robots.txt,
per-host pacing (≥1s between requests to one site), and backoff on 429s.
The crawler never touches private, loopback, link-local, or cloud
metadata addresses.

**Your obligations:** run the code as published (don't weaken the
politeness rules or change the User-Agent), crawl from a public IP on a
network that allows it, read abuse reports your provider forwards, and
keep your token and refresh secret private — if the token leaks, rotate
it (re-run `enroll`).
