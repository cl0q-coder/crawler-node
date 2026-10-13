#!/bin/bash
# Sandbox build for the #274 StormCrawler leg.
#
# Maven Wagon's HTTP client cannot talk through this sandbox's egress proxy,
# so dependencies are resolved with resolve_deps.py (curl-based, honors the
# proxy) into $W/m2repo and compiled with javac directly. On a normal
# network, `mvn -DskipTests package` (pom.xml) produces the same classpath.
set -u -e

HERE=$(cd "$(dirname "$0")" && pwd)
W=${STORMC274_HOME:-$HOME/workspace/stormc274}

export JAVA_HOME=$W/jdk17
if [ ! -x "$JAVA_HOME/bin/javac" ]; then
  echo "JDK 17 not found at $JAVA_HOME (STORMC274_HOME=$W)"; exit 1
fi

echo "== resolving dependencies (curl-based) =="
python3 "$HERE/resolve_deps.py" "$W/m2repo" \
  com.digitalpebble.stormcrawler:storm-crawler-core:2.11 \
  org.apache.storm:storm-client:2.6.0 \
  org.apache.storm:storm-server:2.6.0

echo "== compiling =="
CP=$(cat "$W/m2repo/cp.txt")
mkdir -p "$HERE/target/classes"
# shellcheck disable=SC2046
"$JAVA_HOME"/bin/javac -nowarn -encoding UTF-8 -cp "$CP" -d "$HERE/target/classes" \
  $(find "$HERE/src/main/java" -name "*.java")
cp "$HERE"/src/main/resources/* "$HERE/target/classes/"
echo "BUILD_OK classes=$HERE/target/classes"
