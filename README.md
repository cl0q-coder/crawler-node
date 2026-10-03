# cl0q crawler node

[cl0q](https://cl0q.com) is a free, independent search engine with its own crawler and index. This repository
is that crawler, packaged so other people can run a **node**: it discovers sites, fetches their homepages
politely, and sends what it finds to cl0q's index. The search engine itself is not part of this repository.

The node program is in **early access**: nodes are enrolled by hand. Email
[cl0qsearch@gmail.com](mailto:cl0qsearch@gmail.com) if you'd like to run one.

## How it works

```
seeds.txt ─▶ domain-crawler ─▶ domain_info ─▶ dns-enricher / asn-enricher
                                   │
                                   ▼
                            website-crawler ─▶ website_info ─▶ cl0q-node ─▶ https://cl0q.com/api/ingest
                                                (local Postgres)            (token + enrolled IP)
```

| Service | What it does |
|---|---|
| `domain-crawler` | Follows links from `seeds.txt` and from known domains to discover new domains. |
| `website-crawler` | Fetches each domain's homepage and records its title, description, headings, language, links and response details. |
| `dns-enricher` | Resolves IP, MX and CNAME records for new domains (DNS only). |
| `asn-enricher` | Looks up each site's network (ASN) so requests are spread across hosting providers. Optional. |
| `cl0q-node` | Sends new rows to cl0q with your node's token. |

Everything runs against the node's own Postgres. Nothing listens on a public port.

## Being a good bot

These rules are built into the code. None of them can be switched off, and settings can only make a node slower or
gentler than the limits below, never faster. They're what [cl0q.com/bot](https://cl0q.com/bot) promises site owners.

- **One honest User-Agent**, naming your node:
  `Mozilla/5.0 (compatible; cl0q-node/1.0; operator=<your-node-id>; +https://cl0q.com/bot)`.
  It never imitates a browser and can't be changed.
- **Public internet only.** A node never connects to loopback, private networks, link-local addresses (including
  cloud metadata at 169.254.169.254) or other reserved ranges. The check runs on every connection, after DNS and on
  every redirect, so a page can't steer a node into its own network.
- **robots.txt first, always.** A group for `cl0q-node` or any other cl0q bot name (`cl0q-crawler`,
  `cl0q-discovery`, `cl0q-favicon`, `cl0q`) takes precedence over `*`. Path rules and `Crawl-delay` (capped at 30 s)
  are obeyed.
- **Paced per server.** `website-crawler` leaves at least 1 second between requests to any server IP and any site —
  robots.txt, http fallbacks, redirect hops and retries included. `domain-crawler` leaves at least 2 seconds between
  requests to the same site.
- **Redirects stay on the site.** At most 5 hops, each checked against that host's robots.txt and paced like any
  other request. A redirect to a different site is not followed; that site is queued and crawled on its own terms.
- **Stop when refused.** A dropped connection, reset or timeout counts as a refusal and isn't retried in the same
  cycle. A 429/503 is retried once only if its `Retry-After` is 15 s or less; a longer one is respected by leaving
  the site alone until a later cycle. Repeated refusals pause the whole network or IP group for 1 minute, doubling
  up to 1 hour. `http://` is tried only when a site doesn't offer `https://` at all.
- **Small footprint.** `website-crawler` fetches only homepages, at most once per 120 days. `domain-crawler` fetches
  at most 25 pages per site per cycle. It never fetches login, admin, account, cart, search or query-string URLs, or
  non-HTML files. Pages are capped at 5 MB.
- **Challenge pages aren't kept.** Cloudflare, Imperva, DataDome and similar interstitials are detected, their
  content is thrown away, and they're never sent to cl0q.

## Run a node

You need Docker with Compose v2 and a server whose provider allows crawling. A small VPS (1 vCPU, 2 GB RAM) is
enough. Avoid a home connection: abuse reports from site owners go to the owner of the IP address.

1. **Enroll.** Email us the public IP address your node will crawl from. You'll get a **node id** and a
   **token**. The token only works from that IP.
2. **Configure.**
   ```bash
   cp .env.example .env
   # set CL0Q_NODE_ID, NODE_TOKEN and DB_PASSWORD (and IPINFO_TOKEN if you have one)
   ```
3. **Start.**
   ```bash
   docker compose up -d --build
   docker compose logs -f website-crawler node
   ```
4. **Stop** with `docker compose down`. Each service finishes the work in hand and writes it before exiting, so
   nothing is lost. Your local data stays in the `pgdata` volume. Add `-v` to delete it.

See [docs/CRAWLER_NODES.md](docs/CRAWLER_NODES.md) for the trust model, what is sent to cl0q, and what's
expected of node operators.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `CL0Q_NODE_ID` | — (required) | Your node id; sent in every User-Agent. |
| `NODE_TOKEN` | — (required) | Ingest token from enrollment. |
| `DB_PASSWORD` | — (required) | Password for the node's local Postgres. |
| `INGEST_URL` | `https://cl0q.com` | Where results are sent. Must be https: the token goes with every request. |
| `IPINFO_TOKEN` | empty | Free [ipinfo Lite](https://ipinfo.io/signup) token for ASN grouping. |
| `CRAWL_WORKERS` | `8` | Concurrent homepage fetches. The per-server pacing always applies. |

Everything else is optional and listed in `.env.example`. Politeness settings are floors: a value below a limit
(including zero or a negative number) is raised to it, so they can only make a node gentler. Throughput settings
(workers, connections, timeouts) are kept within sane ranges the same way.

If cl0q's ingest rejects a batch outright (anything but an auth, rate-limit or server error), `cl0q-node` keeps it in
the local `cl0q_node_deadletter` table and carries on with the rows after it; please tell us if that table fills up.

## Development

```bash
go vet ./... && go test ./...
# a local Postgres with initdb/01-schema.sql applied, then e.g. (TARGET is a bare domain; homepages only):
CL0Q_NODE_ID=dev-node DATABASE_URL=postgres://cl0q:cl0q@localhost:5432/cl0q?sslmode=disable \
  TARGET=example.com go run ./cmd/website-crawler
```

Test servers on `127.0.0.1` are refused by the address guard, by design. The package tests switch the guard off
inside the test binary only; there is no flag or environment variable for it.

```
cmd/<service>/   one directory per binary
botid/           the User-Agent and robots.txt names (one place, not configurable)
netguard/        refuses connections to non-public addresses
robotstxt/       the robots.txt rules both crawlers obey
crawlutil/       helpers both crawlers share (registrable site via the public suffix list, https fallback rule)
dbresil/         database-outage helpers (wait for the DB, connect timeouts)
initdb/          the node's database schema
seeds.txt        starting points for discovery
```

## License

[MIT](LICENSE)
