#!/bin/bash
# Nutch 1.23 leg for cl0q #274 (1000 fixture seeds, 1s/host politeness, 16 threads).
#
# Repro: bash run-bench274.sh
#
# What it does:
#   1. Downloads Apache Nutch 1.23 (bin tarball, sha512-verified) into
#      $NUTCH274_HOME (default ~/workspace/nutch274) -- kept OUT of the repo tree.
#   2. Generates a throwaway CA + wildcard *.test cert (openssl) for the fixture.
#   3. Builds fixturelog/ (copy of cl0q/tools/bench-fixture + per-request log).
#   4. Bind-mounts a bench{i}.test -> 127.0.0.1 hosts file over /etc/hosts
#      (needs root; same pattern as the Heritrix leg).
#   5. Runs inject -> generate -> fetch (timed via runstat.py) -> parse ->
#      updatedb, depth 1, then analyzes per-host robots->page gaps.
#
# Local-only bench harness; not for production use.
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
N=${NUTCH274_HOME:-$HOME/workspace/nutch274}
DIST=$N/apache-nutch-1.23
WORK=$N/work
RUNSTAT=$HERE/../runstat.py
NUTCH_VER=1.23
NUTCH_TGZ=apache-nutch-$NUTCH_VER-bin.tar.gz
NUTCH_URL=https://downloads.apache.org/nutch/$NUTCH_VER/$NUTCH_TGZ
# sha512 from https://downloads.apache.org/nutch/1.23/$NUTCH_TGZ.sha512 (2026-10-13)
NUTCH_SHA512=9545589ecb169538d8d0d6e28c26d2841b32dd59582f93cd4e7cdc3d9822f899e3f2bbf23f82315068cb2876f05f9be171f01dfc69b41c6eca6555268ea23c5e

mkdir -p "$N" "$N/tls" "$N/logs"

# --- 0. Nutch dist -----------------------------------------------------------
if [ ! -x "$DIST/bin/nutch" ]; then
  echo "== downloading Nutch $NUTCH_VER =="
  curl -sS --max-time 1200 -o "$N/$NUTCH_TGZ" "$NUTCH_URL"
  echo "$NUTCH_SHA512  $N/$NUTCH_TGZ" | sha512sum -c - || exit 1
  tar --no-same-owner -xzf "$N/$NUTCH_TGZ" -C "$N"
fi

# --- 0b. bench config (written into the dist's conf; dist lives outside repo) --
cat > "$DIST/conf/nutch-site.xml" <<'EOF'
<?xml version="1.0"?>
<?xml-stylesheet type="text/xsl" href="configuration.xsl"?>
<!-- #274 Nutch leg: 1000 fixture hosts, 1s/host politeness floor, 16 threads. -->
<configuration>
  <property><name>http.agent.name</name><value>bench274</value></property>
  <property><name>http.robots.agents</name><value>bench274</value></property>
  <property><name>fetcher.server.delay</name><value>1.0</value></property>
  <property><name>fetcher.threads.fetch</name><value>16</value></property>
  <property><name>fetcher.threads.per.queue</name><value>1</value></property>
  <property><name>http.timeout</name><value>20000</value></property>
  <property><name>http.content.limit</name><value>2097152</value></property>
</configuration>
EOF

# --- 1. throwaway CA + wildcard *.test cert (2-day; fixture presents it) -----
if [ ! -f "$N/tls/server-cert.pem" ]; then
  echo "== generating throwaway fixture CA/cert =="
  (cd "$N/tls" && \
    openssl req -x509 -newkey rsa:2048 -keyout ca-key.pem -out ca-cert.pem \
      -days 2 -nodes -subj "/CN=bench274-fixture-CA" 2>/dev/null && \
    openssl req -newkey rsa:2048 -keyout server-key.pem -out server.csr \
      -nodes -subj "/CN=*.test" 2>/dev/null && \
    printf 'subjectAltName=DNS:*.test,DNS:test\n' > ext.cnf && \
    openssl x509 -req -in server.csr -CA ca-cert.pem -CAkey ca-key.pem \
      -CAcreateserial -out server-cert.pem -days 2 -extfile ext.cnf 2>/dev/null)
fi

# --- 2. logging fixture -------------------------------------------------------
if [ ! -x "$N/fixturelog-bin" ]; then
  echo "== building fixturelog =="
  export PATH=$HOME/go/bin:$PATH
  (cd "$HERE/fixturelog" && go build -o "$N/fixturelog-bin" .) || exit 1
fi

# --- 3. DNS: bind-mount bench{i}.test -> 127.0.0.1 over /etc/hosts ------------
if [ ! -f /tmp/hosts.new ] || ! grep -q "bench999.test" /tmp/hosts.new 2>/dev/null; then
  echo "== generating /tmp/hosts.new =="
  { echo "127.0.0.1 localhost"; for i in $(seq 0 999); do echo "127.0.0.1 bench$i.test"; done; } > /tmp/hosts.new
fi
mount --bind /tmp/hosts.new /etc/hosts
echo "DNS check:"; getent hosts bench0.test; getent hosts bench999.test

# --- 4. no proxies: the fixture is loopback ----------------------------------
unset https_proxy HTTPS_PROXY http_proxy HTTP_PROXY all_proxy ALL_PROXY

# --- 5. fixture ---------------------------------------------------------------
rm -f "$N/requests.log" "$N/fixture-run.log"
(cd "$N" && ./fixturelog-bin -n 1000 -latency 2ms \
  -tls-cert tls/server-cert.pem -tls-key tls/server-key.pem \
  -log requests.log > fixture-run.log 2>&1 & echo $! > /tmp/fixturelog.pid)
sleep 2
PORT=$(grep -oP 'PORT=\K[0-9]+' "$N/fixture-run.log")
echo "fixture on port $PORT"
curl -sk --noproxy '*' --resolve bench0.test:$PORT:127.0.0.1 \
  https://bench0.test:$PORT/robots.txt -o /dev/null -w 'fixture check http=%{http_code}\n'

# --- 6. seeds (port is dynamic; generated per run) ----------------------------
mkdir -p "$WORK/urls"
for i in $(seq 0 999); do echo "https://bench$i.test:$PORT/"; done > "$WORK/urls/seeds.txt"

# --- 7. JVM env ---------------------------------------------------------------
# Java 17 (dist's Hadoop 3.5 works on it; 21 untested). -Xmx1536m: the box is
# RAM-tight (Heritrix used -Xmx2g). preferIPv4Stack: the sandbox misroutes
# IPv4-mapped IPv6 connections to 127/8 (same gotcha as the Heritrix leg).
export NUTCH_JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64
export NUTCH_HEAPSIZE=1536
export NUTCH_OPTS="-Djava.net.preferIPv4Stack=true"
export NUTCH_LOG_DIR=$N/logs

# --- 8. crawl: inject -> generate -> fetch (timed) -> parse -> updatedb ------
rm -rf "$WORK/crawl"
cd "$WORK"
"$DIST/bin/nutch" inject crawl/crawldb urls || exit 1
"$DIST/bin/nutch" generate crawl/crawldb crawl/segments -topN 1000 || exit 1
SEG=$(ls -t crawl/segments | head -1)
echo "segment: $SEG"
python3 "$RUNSTAT" nutch274-fetch "$DIST/bin/nutch" fetch crawl/segments/$SEG || exit 1
"$DIST/bin/nutch" parse crawl/segments/$SEG
"$DIST/bin/nutch" updatedb crawl/crawldb crawl/segments/$SEG

# --- 9. fixture down + gap analysis -------------------------------------------
kill $(cat /tmp/fixturelog.pid) 2>/dev/null
python3 "$HERE/analyze_requests.py" "$N/requests.log"
echo DONE
