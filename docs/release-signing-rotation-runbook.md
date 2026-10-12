# Release-signing key: custody, rotation & compromise runbook

Track U — node update channel. Required to exist BEFORE the first signed
release (Chris's ruling, QA doc 2026-10-11). This runbook governs the
Ed25519 release-signing key that signs `releases.json` and the per-artifact
signatures the node's pinned keys verify (U1 mechanism, U5 pipeline).

## Custody (FINAL — Chris, 2026-10-11; updated per Chris's 2026-10-11 18:45 custody reassignment)

- The private release-signing keys are held by **Dutch** (custodian).
  Deputy signer: **Micah**.
- Two keypairs exist, generated 2026-10-11 by Dutch (crawler-node #252):
  **Key A** signs releases now; **Key B** is the pre-pinned successor so a
  future rotation does not brick nodes that only know A.
- The private keys are NEVER plaintext on the shared CI runner and are never
  posted in any channel, doc, or issue. Only the base64 public halves leave
  Dutch's box (pinned in U1's `DefaultKeyring`, PR #251).
- Private halves are stored with an AES-256-CBC (pbkdf2 100k) encrypted
  backup, round-trip verified. OPERATIONAL NOTE (2026-10-11): the backup
  passphrase is currently co-located on the same host — moving a backup copy
  plus passphrase to Chris's offline storage is outstanding and must happen
  before the first signed release.
- Releases are signed in a **review-gated step**.
- Nodes ship **two pinned public keys** (primary A + secondary B). A key can
  rotate without bricking nodes that only know the old one.

## Planned rotation (non-emergency)

Do this once a year, or whenever the signer roster changes.

1. **Generate the successor keypair** on the custodian's signing machine
   (Dutch, or the named deputy Micah). Never copy the private key to a
   networked host beyond the custodian's.
2. **Overlap-sign.** For one release cycle, sign `releases.json` and the
   artifacts with BOTH the old key and the new key. The manifest carries both
   signatures; U1's verifier accepts either pinned key.
3. **Ship the new pinned key.** The release whose manifest is overlap-signed
   embeds the new pubkey in its pinned-key slot (second slot first, promoted
   to primary on the next rotation). Operators upgrade normally through the
   consent-gated channel — no special action.
4. **Drain and retire.** After the overlap release is the minimum supported
   version across the fleet (check node version signals from enroll/heartbeat),
   stop signing with the old key. Publish a final rotation note in the
   release notes (the egress/permission/privacy callout section also notes
   key changes, per RELEASE-NOTES.md).
5. **Destroy or archive the retired private key** per Chris's call. Archived
   copies, if kept, stay offline with the same custody rules.

## Emergency: suspected compromise

1. **Contain.** Stop all new signing immediately. If the compromised key was
   used for any manifest still in the wild, identify the affected versions.
2. **Yank.** Publish a new manifest (signed with the surviving key) marking
   the affected versions `yanked: true` and raising `min_supported_version`
   past them. Nodes show the loud consent-gated warning; yanked versions are
   never offered again.
3. **Rotate.** Generate a fresh keypair offline, overlap-sign the emergency
   release with the surviving key, and ship the new pinned key in the next
   release per the planned-rotation steps.
4. **Notify.** Post to #war-room and the QA doc: which versions were signed
   with the compromised key, what the replacement is, and the recommended
   operator action (consent to the update). Operators who declined recent
   updates were never exposed — say so explicitly.
5. **Post-mortem.** Record how the key was exposed and what custody change
   prevents recurrence. Chris approves the finding.

## Signing ceremony checklist (every release)

- [ ] Tag review passed (review-gated signing step — a named signer
      approves the release contents, not just the tag).
- [ ] Manifest assembled by the U5 pipeline: per-os/arch artifacts with
      sha256, notes (RELEASE-NOTES.md format), `yanked`, `min_supported_version`.
- [ ] Signed offline with the current private key; signature verified against
      the pinned pubkeys in the previous release's node binary before publish.
- [ ] `releases.json` + artifacts published to Codeberg/Gitea releases and
      the manifest URL.
- [ ] Release notes call out any key-state change (rotation in progress,
      new pinned key shipped).

## What gates what

- **U1** pins the real production keys (#252 public halves, pinned in `DefaultKeyring` by PR #251) — no TEST keys in the shipped path.
- **First signed release / U5 wiring** is blocked on: real keypairs generated
  (done — Dutch, 2026-10-11, #252), deputy signer named (done — Micah),
  offline backup copy + passphrase moved to Chris's offline storage
  (outstanding), and THIS runbook in the repo.
- U5's wiring consumes the real keys already pinned by U1's `DefaultKeyring`
  (PR #251, 9fd836d): the verify path reads the pinned production pubkeys
  directly — nothing to swap, only the release-ceremony flow from this runbook.
