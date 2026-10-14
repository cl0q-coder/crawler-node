# cl0q crawler node on DigitalOcean App Platform

`app.yaml` maps the docker-compose stack to App Platform components: five
workers (one per crawler binary, same Docker image, different run command)
plus a Postgres database. No code changes were needed — every binary is
already fully env-configurable.

## Deploy steps (one time)

1. **Connect GitHub.** In the DO console, connect the `cl0q-coder/crawler-node`
   repo (DO's GitHub app needs authorizing — team owner job).
2. **Create the app** from `.do/app.yaml` (`doctl apps create --spec .do/app.yaml`
   or console → Apps → Create → paste the spec).
3. **Turn OFF Autodeploy on every component.** A push restarting all five
   workers mid-crawl is the App Platform version of the disk-full death.
   Deploy manually when you mean it.
4. **Load the schema once.** Get the database connection string from the
   `crawlerdb` component and run:
   `psql "<connection-string>" < initdb/01-schema.sql`
   (The crawlers self-ensure later migration columns at startup, so this is
   the only manual DB step.)
5. **Set the secrets** (encrypted env vars, console or `doctl`):
   - `NODE_TOKEN` on the `node` worker — from the enrolled node's
     `cl0q-node.json` (node `nd_996e7290`, contact cl0qsearch@gmail.com).
   - `IPINFO_TOKEN` on `asn-enricher` if you have one (optional).
6. **Deploy.** Watch the runtime logs; `website-crawler` should show fetches
   within a minute.

## Cost

About **$25/mo**: 5 × basic worker (`apps-s-1vcpu-0.5gb`) + dev database
(free). A production managed Postgres instead of the dev database adds
about $15/mo. The droplet node it replaces costs $6/mo — the App version
trades money for zero server maintenance.

## Notes and limits

- **No persistent disk.** The database lives in `crawlerdb`, so worker
  restarts are safe. `NODE_MAX_DB_GB=4` keeps the dev database small;
  raise it if you move to a managed cluster.
- **Enrollment is already done** (`nd_996e7290`). The token goes in as a
  secret env var — no `cl0q-node.json` needed on the platform.
- **Open question (with Claude):** confirm the node token is NOT IP-locked.
  App Platform egress IPs are dynamic; if the token only works from the
  enrollment IP, ingest auth fails and this whole path is dead. (See
  `.env.example` vs `docs/NODE_OPERATORS.md` — they contradict each other.)
- The GitHub mirror lags Gitea `main` by ~a day; the app deploys from the
  mirror. For deploys that must track `main` exactly, push the mirror first.
