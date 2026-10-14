#!/bin/bash
# Heritrix 3 phase-3 run for cl0q #274 (1000 fixture seeds, 1s/host politeness, 16 threads).
# Repro: bash run-bench274.sh   (fixture server must be on :40659, /tmp/hosts.new present)
set -u
mount --bind /tmp/hosts.new /etc/hosts
echo "DNS check:"; getent hosts bench0.test; getent hosts bench999.test
unset https_proxy HTTPS_PROXY http_proxy HTTP_PROXY all_proxy ALL_PROXY
export JAVA_OPTS="-Xmx2g -Djava.net.preferIPv4Stack=true"
cd $HOME/workspace/heritrix3/heritrix-3.18.0
python3 $HOME/workspace/cl0q-274/crawler-node/tools/274-comparison/runstat.py \
  heritrix3-bench274 \
  ./bin/heritrix -a admin:bench274pw -b 127.0.0.1 -p 8443 -r bench274
