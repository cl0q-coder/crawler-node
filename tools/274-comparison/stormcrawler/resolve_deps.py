#!/usr/bin/env python3
"""Mini Maven transitive dependency resolver (sandbox proxy workaround).

Maven Wagon's HTTP client chokes on the sandbox egress proxy, but curl works
fine. This resolves the transitive closure of the given root coordinates by
parsing POMs (parent chain + dependencyManagement + property interpolation)
and downloads jars into a Maven-layout local repo. Compile scope + runtime
scope only; test/provided/optional skipped; nearest-wins on conflicts.

Usage: resolve_deps.py <repo-dir> <group:artifact:version> [...]
Writes <repo-dir>/cp.txt with the ':'-joined jar classpath.
"""
import os
import re
import subprocess
import sys
import xml.etree.ElementTree as ET

REPO = sys.argv[1]
ROOTS = sys.argv[2:]
PROXY = "http://hatch-egress-proxy:3128"
BASE = "https://repo1.maven.org/maven2"


def curl(url, out):
    if os.path.exists(out) and os.path.getsize(out) > 0:
        return True
    tmp = out + ".part"
    os.makedirs(os.path.dirname(out), exist_ok=True)
    r = subprocess.run(
        ["curl", "-s", "--max-time", "180", "--proxy", PROXY,
         "-o", tmp, "-w", "%{http_code}", url],
        capture_output=True, text=True)
    if r.stdout.strip() == "200" and os.path.exists(tmp) and os.path.getsize(tmp) > 0:
        os.rename(tmp, out)
        return True
    if os.path.exists(tmp):
        os.remove(tmp)
    return False


def repo_path(group, artifact, version, ext):
    return os.path.join(REPO, group.replace(".", "/"), artifact, version,
                        f"{artifact}-{version}.{ext}")


def strip_ns(root):
    for e in root.iter():
        if "}" in e.tag:
            e.tag = e.tag.split("}", 1)[1]
    return root


def fetch_pom(group, artifact, version):
    p = repo_path(group, artifact, version, "pom")
    url = (f"{BASE}/{group.replace('.', '/')}/{artifact}/{version}/"
           f"{artifact}-{version}.pom")
    return p if curl(url, p) else None


pom_cache = {}


def txt(elem, name):
    e = elem.find(name)
    return e.text.strip() if e is not None and e.text else None


def load_pom(group, artifact, version):
    key = (group, artifact, version)
    if key in pom_cache:
        return pom_cache[key]
    path = fetch_pom(group, artifact, version)
    if not path:
        pom_cache[key] = None
        return None
    root = strip_ns(ET.parse(path).getroot())

    parent = None
    pe = root.find("parent")
    if pe is not None:
        parent = load_pom(txt(pe, "groupId"), txt(pe, "artifactId"),
                          txt(pe, "version"))

    props = {}
    if parent:
        props.update(parent["props"])
    pel = root.find("properties")
    if pel is not None:
        for e in pel:
            props[e.tag] = (e.text or "").strip()

    def interp(s):
        if not s:
            return s
        def sub(mo):
            k = mo.group(1)
            if k == "project.version":
                return version
            if k == "project.groupId":
                return group
            if k == "project.artifactId":
                return artifact
            return props.get(k, mo.group(0))
        prev = None
        while prev != s:
            prev, s = s, re.sub(r"\$\{([^}]+)\}", sub, s)
        return s

    g = interp(txt(root, "groupId") or (parent["group"] if parent else group))
    a = interp(txt(root, "artifactId") or artifact)
    v = interp(txt(root, "version") or (parent["version"] if parent else version))

    mgmt = {}
    if parent:
        mgmt.update(parent["mgmt"])
    dml = root.find("dependencyManagement")
    if dml is not None:
        for d in dml.findall("dependencies/dependency"):
            dg, da = interp(txt(d, "groupId")), interp(txt(d, "artifactId"))
            dv = interp(txt(d, "version"))
            dtype = txt(d, "type") or "jar"
            dscope = txt(d, "scope") or "compile"
            if dscope == "import" and dtype == "pom" and dv and "${" not in dv:
                # BOM import: merge its managed versions
                bom = load_pom(dg, da, dv)
                if bom:
                    for mk, mv in bom["mgmt"].items():
                        mgmt.setdefault(mk, mv)
                continue
            if dv:
                mgmt[(dg, da)] = dv

    deps = []
    dl = root.find("dependencies")
    if dl is not None:
        for d in dl.findall("dependency"):
            dg, da = interp(txt(d, "groupId")), interp(txt(d, "artifactId"))
            dv = interp(txt(d, "version"))
            if not dv and (dg, da) in mgmt:
                dv = mgmt[(dg, da)]
            if not dv:
                print(f"WARN no version for {dg}:{da}, skipped", file=sys.stderr)
                continue
            scope = txt(d, "scope") or "compile"
            optional = txt(d, "optional") == "true"
            excl = set()
            el = d.find("exclusions")
            if el is not None:
                for e in el.findall("exclusion"):
                    excl.add((interp(txt(e, "groupId")), interp(txt(e, "artifactId"))))
            deps.append({"g": dg, "a": da, "v": interp(dv), "scope": scope,
                         "optional": optional, "excl": excl,
                         "type": txt(d, "type") or "jar"})

    info = {"group": g, "artifact": a, "version": v, "props": props,
            "mgmt": mgmt, "deps": deps,
            "packaging": txt(root, "packaging") or "jar"}
    pom_cache[key] = info
    return info


resolved = {}
order = []


def visit(g, a, v, excluded):
    key = (g, a)
    if key in excluded or key in resolved:
        return
    info = load_pom(g, a, v)
    if not info:
        print(f"WARN could not load pom {g}:{a}:{v}", file=sys.stderr)
        return
    resolved[key] = info["version"]
    order.append((info["group"], info["artifact"], info["version"]))
    for d in info["deps"]:
        if d["scope"] in ("test", "provided", "system"):
            continue
        if d["optional"] or d["type"] != "jar":
            continue
        if (d["g"], d["a"]) in d["excl"]:
            continue
        visit(d["g"], d["a"], d["v"], excluded | d["excl"])


for root in ROOTS:
    g, a, v = root.split(":")
    visit(g, a, v, set())

ok, fail = 0, []
for g, a, v in order:
    info = pom_cache.get((g, a, v))
    if info and info["packaging"] == "pom":
        continue
    jp = repo_path(g, a, v, "jar")
    url = (f"{BASE}/{g.replace('.', '/')}/{a}/{v}/{a}-{v}.jar")
    if curl(url, jp):
        ok += 1
    else:
        fail.append(f"{g}:{a}:{v}")

print(f"artifacts: {len(order)}, jars downloaded: {ok}, failed: {len(fail)}")
for f in fail:
    print("FAILED:", f)
cps = [repo_path(g, a, v, "jar") for g, a, v in order
       if os.path.exists(repo_path(g, a, v, "jar"))]
with open(os.path.join(REPO, "cp.txt"), "w") as f:
    f.write(":".join(cps))
print("cp entries:", len(cps))
