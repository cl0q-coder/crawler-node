#!/bin/bash
# sync-vendor.sh: Sync vendored packages from standalone repos at pinned commits.
# Usage: ./sync-vendor.sh [package]  (no arg = all)
# Updates vendor-pins.txt with the new SHAs.
set -e
cd "$(dirname "$0")"

PINS="vendor-pins.txt"
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

sync_one() {
  local pkg=$1
  local sha=$2
  local url=$3
  echo "Syncing $pkg from $url @ $sha..."
  git clone -q "$url" "$TMPDIR/$pkg" 2>/dev/null || {
    # Fallback to local checkout if remote fails
    cp -r ~/workspace/gitea-repos/$pkg "$TMPDIR/$pkg"
  }
  (cd "$TMPDIR/$pkg" && git checkout -q "$sha") || {
    echo "ERROR: cannot check out $sha for $pkg" >&2
    exit 1
  }
  # Copy Go files only (not .git, .github, docs)
  rm -rf "$pkg"
  mkdir -p "$pkg"
  cp "$TMPDIR/$pkg"/*.go "$pkg/" 2>/dev/null || true
  # Copy testdata/ when present (e.g. robotstxt differential corpus)
  if [ -d "$TMPDIR/$pkg/testdata" ]; then
    cp -r "$TMPDIR/$pkg/testdata" "$pkg/testdata"
  fi
  echo "  $pkg synced"
}

if [ -n "$1" ]; then
  # Sync single package (uses current pin, or latest if not pinned)
  line=$(grep "^$1 " "$PINS" || echo "")
  if [ -n "$line" ]; then
    sync_one $line
  else
    echo "Package $1 not in $PINS"
    exit 1
  fi
else
  # Sync all
  while read -r pkg sha url; do
    [[ "$pkg" =~ ^#.*$ ]] && continue
    [ -z "$pkg" ] && continue
    sync_one "$pkg" "$sha" "$url"
  done < "$PINS"
fi

echo "Done. Run 'go build ./...' to verify."
