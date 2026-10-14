#!/usr/bin/env python3
"""Generate the Heritrix 3 job for #274 phase 3 from the default profile.

Writes jobs/bench274/crawler-beans.cxml under the Heritrix home with:
  - 1000 fixture seed URLs inlined
  - crawlController.maxToeThreads = 16
  - disposition delayFactor=1.0, minDelayMs=1000, maxDelayMs=60000
    (1s per-host floor, same binding constraint as the crawler-node run)
  - TooManyHopsDecideRule maxHops=0 (seeds only; fixture outlinks would
    otherwise explode the crawl 4x)
  - fetchHttp sslTrustLevel OPEN (default; fixture uses throwaway CA)

Usage: gen_job.py <heritrix-home> <port>
"""
import re
import sys

home, port = sys.argv[1], sys.argv[2]
src = f"{home}/../profile/org/archive/crawler/restlet/profile-crawler-beans.cxml"
xml = open(src).read()

seeds = "\n".join(f"https://bench{i}.test:{port}/" for i in range(1000))

# 0. Valid crawl metadata via the simpleOverrides properties block (this
#    takes precedence over the bean properties below it).
xml = xml.replace(
    "metadata.operatorContactUrl=ENTER_AN_URL_WITH_YOUR_CONTACT_INFO_HERE_FOR_WEBMASTERS_AFFECTED_BY_YOUR_CRAWL",
    "metadata.operatorContactUrl=https://example.com/heritrix-contact",
)
xml = xml.replace(
    "metadata.jobName=basic",
    "metadata.jobName=bench274",
)
xml = xml.replace(
    "metadata.description=Basic crawl starting with useful defaults",
    "metadata.description=cl0q #274 phase-3 fixture benchmark: 1000 hosts, 1s politeness floor",
)

# 1. Inline seeds via the longerOverrides seeds.textSource.value prop (the
#    sanctioned injection point; overrides the bean property).
xml = re.sub(
    r'(<prop key="seeds.textSource.value">).*?(</prop>)',
    lambda m: m.group(1) + "\n" + seeds + "\n    " + m.group(2),
    xml,
    flags=re.S,
)
# Belt-and-braces: also replace the bean-level ConfigString value.
xml = re.sub(
    r'(<bean id="seeds" class="org.archive.modules.seeds.TextSeedModule">.*?<property name="value">\s*<value>).*?(</value>)',
    lambda m: m.group(1) + "\n" + seeds + "\n        " + m.group(2),
    xml,
    flags=re.S,
)

# 2. 16 fetch threads.
xml = xml.replace(
    '<!-- <property name="maxToeThreads" value="25" /> -->',
    '<property name="maxToeThreads" value="16" />',
)

# 3. 1s per-host politeness floor.
xml = xml.replace(
    '<!-- <property name="delayFactor" value="5.0" /> -->',
    '<property name="delayFactor" value="1.0" />',
)
xml = xml.replace(
    '<!-- <property name="minDelayMs" value="3000" /> -->',
    '<property name="minDelayMs" value="1000" />',
)
xml = xml.replace(
    '<!-- <property name="maxDelayMs" value="30000" /> -->',
    '<property name="maxDelayMs" value="60000" />',
)

# 4. Seeds only: reject anything more than 0 hops out.
xml = xml.replace(
    '<!-- <property name="maxHops" value="20" /> -->',
    '<property name="maxHops" value="0" />',
)

# 5. Trust the throwaway fixture CA (Heritrix default is OPEN anyway;
#    make it explicit).
xml = xml.replace(
    '<!-- <property name="sslTrustLevel" value="OPEN" /> -->',
    '<property name="sslTrustLevel" value="OPEN" />',
)

import os

jobdir = f"{home}/jobs/bench274"
os.makedirs(jobdir, exist_ok=True)
open(f"{jobdir}/crawler-beans.cxml", "w").write(xml)

checks = [
    ("metadata override", "metadata.operatorContactUrl=https://example.com/heritrix-contact" in xml),
    ("seeds in longerOverrides", seeds.splitlines()[0] in xml.split('<prop key="seeds.textSource.value">')[1].split("</prop>")[0]),
    ("maxToeThreads=16", 'maxToeThreads" value="16"' in xml),
    ("minDelayMs=1000", 'minDelayMs" value="1000"' in xml),
    ("delayFactor=1.0", 'delayFactor" value="1.0"' in xml),
    ("maxHops=0", 'maxHops" value="0"' in xml),
    ("sslTrustLevel OPEN", 'sslTrustLevel" value="OPEN"' in xml),
]
for name, ok in checks:
    print(("OK  " if ok else "FAIL") + " " + name)
assert all(ok for _, ok in checks), "job generation failed"
print("wrote", jobdir + "/crawler-beans.cxml")
