# Brand favicon seed — #111 v1

`brand_favicons.json` is the curated brand fingerprint seed for
favicon brand-impersonation detection (`cmd/favicon-scan`).

## Format

JSON array of:
```json
{
  "brand_name": "PayPal",
  "domain": "paypal.com",
  "sha256": "<hex of raw favicon bytes>",
  "ahash": "<16-hex-char 64-bit average hash, optional>",
  "source": "seedgen 2026-10-09 from <icon URL>"
}
```

## Regenerating

The seed was generated 2026-10-09 by fetching each brand's live favicon
(homepage `<link rel=icon>` discovery, `/favicon.ico` fallback) and
computing SHA256 + aHash. 8 of 20 candidate brands were skipped (bot
protection: Instagram, X, GitHub, Dropbox, Meta, Facebook, Binance, DHL).

To regenerate or extend: fetch the brand's favicon bytes, SHA256 them,
optionally compute the aHash with the same 8x8 box-average algorithm as
`cmd/favicon-scan/ahash.go`, and append an entry. Keep `domain` as the
brand's canonical domain — `isBrandDomain` compares registrable domains,
so `login.paypal.com` is the brand, not a hit.

## Maintenance

Top-50 phished brands (APWG-style lists) is the target set; v1 ships 12.
Whoever curates adds entries here and the seed is versioned with the repo.
