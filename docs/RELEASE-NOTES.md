# Release-notes standard (TRACK U6)

Every release of the crawler node ships **operator-facing release notes**.
These notes are written for the person running the node — not the person
who wrote the code — and they are the exact text the operator sees when
they run `cl0q-node update check` (TRACK U). A release candidate tag is
not cut until its notes are written and reviewed.

Notes live in `docs/releases/vX.Y.Z.md` and are linked from the
`CHANGELOG.md` entry for that release.

## Required sections

Every release-notes file has these five sections, in this order.
None may be omitted — if a section has nothing to say, say so explicitly.

### 1. Summary

One paragraph: what this release is, in operator terms. Who should care,
and how strongly: **recommended** (upgrade when convenient),
**strongly recommended** (fixes something you're likely hitting), or
**security** (upgrade promptly — and say why, plainly).

### 2. What changed

What changed from the operator's point of view: new behaviour, changed
defaults, new or removed knobs, fixes to things that were broken. Lead
with impact, not implementation. Link the full `CHANGELOG.md` entry for
the developer-level detail.

### 3. Egress / permission / privacy changes

An explicit callout of anything this release changes about what the node
touches on the network, what it is allowed to do, or what data it sends
or stores. New outbound endpoints, new ports, new stored data, new
telemetry, changed User-Agent — all of it goes here.

**If there are none, write "No egress, permission, or privacy changes."
Do not leave the section out.** The point is that the operator can trust
a missing change because the section is always present.

### 4. Upgrade steps

Concrete steps: pull/build, redeploy, anything the operator must run by
hand (migrations, re-enrol, config changes). If nothing manual is needed,
say so. If the release needs a new CI runner or server-side change first
(TRACK U U5), say that here too.

### 5. Rollback steps

Concrete steps to get back to the previous version, and an honest note
about what regresses by doing so. "Rollback reintroduces bug X" is a
valid and necessary sentence.

## Writing bar

- Plain language. An operator who has never read the code should
  understand every sentence. Jargon gets a one-line explanation.
- Short enough to read in a terminal: this is the text `update check`
  prints. A page, not a chapter.
- Honest about risk: never "minor improvements" when behaviour changed;
  never silent about a fix the operator was waiting for.

## Discipline

- The author of the release candidate writes the notes in the same
  review cycle — notes are reviewed like code.
- No release tag without notes. The tag step checks for
  `docs/releases/vX.Y.Z.md`.
- Backfilled releases (v0.1.0, v0.1.1) are written from the changelog
  and the tagged tree, marked as backfilled at the top.
