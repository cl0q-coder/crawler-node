# Crawler nodes

A node crawls from its operator's server and sends the results to cl0q. It never holds cl0q's database
credentials, and cl0q can stop accepting a node's data at any time.

## Trust model

```
crawler node ──HTTPS, Bearer <token>──▶  cl0q ingest (/api/ingest/*)  ──▶  cl0q index
 (own Postgres, no cl0q credentials)     checks the token AND the source IP,
                                         re-applies cl0q's own filters,
                                         records which node sent each row
```

A node is authorized by two things, and both must match:

1. **Token**: issued once at enrollment. cl0q stores only its SHA-256 hash.
2. **Source IP**: the address (or range) the node was enrolled with. A token used from anywhere else is
   rejected.

cl0q treats node data as untrusted input. It re-applies its own blacklist and domain rules, caps request
sizes, and stamps every row with the node that sent it. If a node misbehaves, cl0q revokes it and can remove
everything it contributed in one step.

## What a node sends

`cl0q-node` forwards new rows from two local tables:

- **website_info**: one row per site. It holds the homepage's title, description, keywords, canonical URL,
  first `<h1>`, status code, content type and length, response time, redirect count, number of outbound links
  and images, word count, charset, robots meta, Open Graph title and image, Twitter card, page language,
  `Server` header, fetch error and the ETag / Last-Modified validators. Page body text is never stored or sent.
- **domain_info**: one row per discovered domain. It holds the domain, its IPv4 and IPv6 addresses, MX and CNAME
  records, and the page it was found on.

Pages that turn out to be bot-challenge interstitials, and names that resolve only to private or other non-public
addresses (which are never contacted), are not sent.

Apart from the node id in the User-Agent (see below), nothing about the node's operator or other traffic on the
node's server is collected or sent to cl0q. Two outside services do see part of a node's work, as they would for any
crawler:

- **Public DNS resolvers** (Cloudflare 1.1.1.1 / 1.0.0.1, Google 8.8.8.8 / 8.8.4.4, Quad9 9.9.9.9) receive the domain
  names the node looks up.
- **ipinfo.io**, only if you set `IPINFO_TOKEN`, receives the IP addresses `asn-enricher` looks up.

Known limit: rows are tracked by id, so a page the node re-crawls later (same row) isn't sent again. If cl0q rejects a
batch outright, the node keeps it in its local `cl0q_node_deadletter` table and carries on; nothing in it is sent
again automatically. Results go to `INGEST_URL` over https only, and the node never follows a redirect with its token.

## What site owners see

Every request carries:

```
Mozilla/5.0 (compatible; cl0q-node/1.0; operator=<node-id>; +https://cl0q.com/bot)
```

A site owner can block all nodes with one robots.txt group:

```
User-agent: cl0q-node
Disallow: /
```

Groups for any other cl0q bot name (`cl0q-crawler`, `cl0q-discovery`, `cl0q-favicon`, `cl0q`) bind nodes too. If a
node causes trouble, a site owner can send us its operator id, and we can contact that operator or revoke the node.

## What we expect from node operators

- Run the code as published. Don't remove or weaken its politeness rules, and don't change the User-Agent.
- Crawl from the IP you enrolled, on a network whose provider allows crawling.
- Read the abuse reports your provider forwards to you, and tell us about them
  ([cl0qsearch@gmail.com](mailto:cl0qsearch@gmail.com)). Stop your node (`docker compose down`) if asked.
- Keep your token private. If it leaks, tell us and we'll issue a new one.

## Ingest endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/ingest/ping` | Check that the token and source IP are accepted |
| POST | `/api/ingest/website` | Send `website_info` rows (`{"rows":[…]}`, at most 2000 per request) |
| POST | `/api/ingest/domains` | Send `domain_info` rows |
