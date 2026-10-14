#!/bin/bash
# StormCrawler 2.11 leg for cl0q #274 (1000 fixture seeds, 1s/host politeness, 16 fetcher bolts).
#
# Repro: bash run-bench274.sh [N]   (N = seeds, default 1000; use a small N for smoke tests)
#
# What it does:
#   1. Builds the bench topology classes (bench274.Bench274) via build.sh
#      (curl-based dependency resolution + javac; Maven Wagon can't use the
#      sandbox egress proxy — on a normal network `mvn -DskipTests package`
#      from pom.xml works too).
#   2. Generates a throwaway CA + wildcard *.test cert (openssl) for the fixture.
#   3. Builds fixturelog/ (copy of cl0q/tools/bench-fixture + per-request log;
#      shared with the Nutch leg at ../nutch/fixturelog).
#   4. Bind-mounts a bench{i}.test -> 127.0.0.1 hosts file over /etc/hosts
#      (needs root; same pattern as the Heritrix/Nutch legs).
#   5. Runs the local-mode topology (timed via runstat.py), then analyzes
#      per-host robots->page gaps from the fixture request log.
#
# Local-only bench harness; not for production use.
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
N=${1:-1000}
W=${STORMC274_HOME:-$HOME/workspace/stormc274}
RUNSTAT=$HERE/../runstat.py
FIXTURELOG_SRC=$HERE/../nutch/fixturelog

mkdir -p "$W" "$W/tls" "$W/logs"

# --- 0. bench classes -------------------------------------------------------------
# Sandbox build: Maven Wagon can't use the egress proxy, so resolve_deps.py
# (curl-based) seeds $W/m2repo and javac compiles directly. See build.sh.
# On a normal network `mvn -DskipTests package` (pom.xml) works too.
if [ ! -f "$HERE/target/classes/bench274/Bench274.class" ]; then
  echo "== building bench classes =="
  bash "$HERE/build.sh" || exit 1
fi
CP="$HERE/target/classes:$(cat "$W/m2repo/cp.txt")"

# --- 1. throwaway CA + wildcard *.test cert (2-day; fixture presents it) ------
if [ ! -f "$W/tls/server-cert.pem" ]; then
  echo "== generating throwaway fixture CA/cert =="
  (cd "$W/tls" && \
    openssl req -x509 -newkey rsa:2048 -keyout ca-key.pem -out ca-cert.pem \
      -days 2 -nodes -subj "/CN=bench274-fixture-CA" 2>/dev/null && \
    openssl req -newkey rsa:2048 -keyout server-key.pem -out server.csr \
      -nodes -subj "/CN=*.test" 2>/dev/null && \
    printf 'subjectAltName=DNS:*.test,DNS:test\n' > ext.cnf && \
    openssl x509 -req -in server.csr -CA ca-cert.pem -CAkey ca-key.pem \
      -CAcreateserial -out server-cert.pem -days 2 -extfile ext.cnf 2>/dev/null)
fi

# --- 2. logging fixture ---------------------------------------------------------
if [ ! -x "$W/fixturelog-bin" ]; then
  echo "== building fixturelog =="
  export PATH=$HOME/go/bin:$PATH
  (cd "$FIXTURELOG_SRC" && go build -o "$W/fixturelog-bin" .) || exit 1
fi

# --- 3. DNS: bind-mount bench{i}.test -> 127.0.0.1 over /etc/hosts -------------
# NOTE: the hosts file lives in $W (persistent), NOT /tmp (/tmp is wiped
# between sessions on this box).
if [ ! -f "$W/hosts.new" ]; then
  echo "== generating $W/hosts.new =="
  { echo "127.0.0.1 localhost"; for i in $(seq 0 999); do echo "127.0.0.1 bench$i.test"; done; } > "$W/hosts.new"
fi
mount --bind "$W/hosts.new" /etc/hosts
echo "DNS check:"; getent hosts bench0.test; getent hosts bench999.test

# --- 4. no proxies: the fixture is loopback --------------------------------------
unset https_proxy HTTPS_PROXY http_proxy HTTP_PROXY all_proxy ALL_PROXY

# --- 5. fixture ------------------------------------------------------------------
rm -f "$W/requests.log" "$W/fixture-run.log"
(cd "$W" && ./fixturelog-bin -n "$N" -latency 2ms \
  -tls-cert tls/server-cert.pem -tls-key tls/server-key.pem \
  -log requests.log > fixture-run.log 2>&1 & echo $! > fixturelog.pid)
sleep 2
PORT=$(grep -oP 'PORT=\K[0-9]+' "$W/fixture-run.log")
echo "fixture on port $PORT"
curl -sk --noproxy '*' --resolve bench0.test:$PORT:127.0.0.1 \
  https://bench0.test:$PORT/robots.txt -o /dev/null -w 'fixture check http=%{http_code}\n'

# --- 6. seeds (port is dynamic; generated per run) ---------------------------------
for i in $(seq 0 $((N-1))); do echo "https://bench$i.test:$PORT/"; done > "$W/seeds.txt"

# --- 7. JVM env --------------------------------------------------------------------
# Portable JDK 17 from the shared workspace (Storm 2.6.0 supports 11/17).
# preferIPv4Stack: the sandbox misroutes IPv4-mapped IPv6 connections to 127/8
# (same gotcha as the Heritrix/Nutch legs).
export JAVA_HOME=$W/jdk17

# --- 8. crawl (timed) ---------------------------------------------------------------
python3 "$RUNSTAT" stormc274-fetch "$JAVA_HOME/bin/java" \
  -Djava.net.preferIPv4Stack=true -Xmx1536m \
  -cp "$CP" bench274.Bench274 "$W/seeds.txt" "$N" 16 2>&1 | tee "$W/bench.log"

# --- 9. fixture down + gap analysis ---------------------------------------------------
kill $(cat "$W/fixturelog.pid") 2>/dev/null
python3 "$HERE/../nutch/analyze_requests.py" "$W/requests.log"
echo DONE
