-- cl0q crawler node — local database schema.
-- The postgres image applies this once, on the first start of an empty volume (docker-entrypoint-initdb.d).
-- It holds only what this node's crawlers and its forwarder (cl0q-node) use; the search index lives at cl0q.

-- Every domain the node knows about. domain-crawler and website-crawler (outbound links) add rows,
-- dns-enricher fills the DNS columns, asn-enricher stamps asn_checked_at, website-crawler stamps web_crawled_at.
CREATE TABLE domain_info (
    id                bigserial PRIMARY KEY,
    domain            text,
    ip_addresses_ipv4 text,                       -- comma-separated; the first one is the "primary" IP
    ip_addresses_ipv6 text,
    mx_records        text,
    cname             text,
    found_on          text,                       -- the page this domain was discovered on
    crawled_at        timestamp,                  -- when it was discovered
    dns_checked_at    timestamptz,
    dns_fail_count    integer NOT NULL DEFAULT 0, -- 3 = DNS-dead, skipped by the crawlers
    asn_checked_at    timestamptz,
    web_crawled_at    timestamptz                 -- last website-crawler attempt (any outcome)
);
-- Non-unique on purpose: writers dedupe with WHERE NOT EXISTS, and a unique violation would fail a whole batch.
CREATE INDEX idx_domain_info_domain ON domain_info (domain);
-- The crawl frontier (never attempted, DNS not dead), read in one index-only pass by website-crawler.
CREATE INDEX idx_domain_info_crawl_todo ON domain_info (id) INCLUDE (domain, ip_addresses_ipv4)
    WHERE web_crawled_at IS NULL AND dns_fail_count < 3;
CREATE INDEX idx_domain_info_web_crawled_at ON domain_info (web_crawled_at);   -- recrawl-due range scan
CREATE INDEX idx_domain_info_dns_todo ON domain_info (id)
    WHERE coalesce(ip_addresses_ipv4, '') = '' AND dns_checked_at IS NULL;
CREATE INDEX idx_domain_info_dns_recheck ON domain_info (dns_checked_at)
    WHERE coalesce(ip_addresses_ipv4, '') = '' AND dns_checked_at IS NOT NULL AND dns_fail_count < 3;
CREATE INDEX idx_domain_info_asn_todo ON domain_info (id)
    WHERE asn_checked_at IS NULL AND ip_addresses_ipv4 IS NOT NULL;

-- One row per site: what website-crawler found on its homepage.
CREATE TABLE website_info (
    id             bigserial PRIMARY KEY,
    domain         text NOT NULL UNIQUE,
    title          text,
    description    text,
    keywords       text,
    canonical      text,
    h1             text,
    status_code    integer,
    content_type   text,
    content_length bigint,
    latency_ms     bigint,
    redirects      integer,
    outbound_links integer,
    image_count    integer,
    word_count     integer,
    charset        text,
    robots         text,
    og_title       text,
    og_image       text,
    twitter_card   text,
    page_language  text,
    server_header  text,
    fetch_error    text,                          -- why a fetch failed or was refused (e.g. robots, challenge:<vendor>)
    etag           text,                          -- conditional-GET validators for the next recrawl
    last_modified  text,
    crawled_at     timestamp NOT NULL DEFAULT now(),
    source_node    text
);

-- Network / country of each primary IP (asn-enricher, ipinfo Lite). website-crawler batches by org.
CREATE TABLE asninfo (
    ip                      text PRIMARY KEY,
    loc                     text,
    org                     text,
    city                    text,
    postal                  text,
    region                  text,
    country                 text,
    timezone                text,
    continent_code          text,
    continent_name          text,
    country_flag_emoji      text,
    country_flag_unicode    text,
    country_name            text,
    country_currency_code   text,
    country_currency_symbol text,
    country_flag_url        text
);

-- Sites never to store. kind = 'domain' (an exact apex and its subdomains) or 'keyword' (a whole dot/hyphen label).
CREATE TABLE cl0q_blacklist (
    id       serial PRIMARY KEY,
    keyword  text NOT NULL UNIQUE,
    kind     text NOT NULL DEFAULT 'keyword',
    note     text,
    added_at timestamptz NOT NULL DEFAULT now()
);

-- Runtime switches (e.g. key 'crawl_subdomains' = 'on' lets the crawlers follow subdomains; default off).
CREATE TABLE cl0q_settings (
    key   text PRIMARY KEY,
    value text
);

-- cl0q-node's forwarding watermark: the last row id sent, per table.
CREATE TABLE cl0q_node_state (
    name    text PRIMARY KEY,
    last_id bigint NOT NULL DEFAULT 0
);

-- Batches the ingest rejected for good (a 4xx other than 401/403/408/429), kept for inspection instead of blocking
-- every row behind them.
CREATE TABLE cl0q_node_deadletter (
    id         bigserial PRIMARY KEY,
    tbl        text NOT NULL,
    first_id   bigint NOT NULL,
    last_id    bigint NOT NULL,
    status     int NOT NULL,
    error      text,
    rows       jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
