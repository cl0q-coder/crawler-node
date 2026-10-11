# Contributing

Thanks for helping. This file is the working agreement for everyone who changes this code.

## Before you open a pull request

```bash
gofmt -l .                  # must print nothing
go vet ./...
go test -race -count=1 ./...
```

CI runs the same checks, applies `initdb/01-schema.sql` to a fresh Postgres and builds the Docker image. A pull
request merges only when CI is green.

## How changes flow

- `main` is always releasable. Work on a branch named for what it does (`fix/robots-groups`, `test/netguard-iana`),
  then open a pull request against `main`.
- One topic per pull request. Say what changed, why, and how you tested it (commands and output, or the test names).
- Every bug fix comes with a test that fails without it.
- Release candidates are tags (`rc3`, `rc4`, …). A release candidate is the exact tree that was reviewed and tested;
  changes after it go into the next one.
- Every release ships operator-facing release notes in `docs/releases/vX.Y.Z.md`, written to the standard in
  `docs/RELEASE-NOTES.md` (summary, what changed, explicit egress/permission/privacy callout, upgrade steps,
  rollback steps). No release tag without notes — the tag step checks for the file.

## Rules that don't bend

- **Politeness stays.** Don't remove or weaken robots.txt handling, the per-site and per-server pacing floors, the
  User-Agent, or the non-public address guard (`netguard`). A change may only make the crawler gentler.
- **No secrets in the repository**: no tokens, passwords, private hostnames or IP addresses, in code, tests, docs or
  commit messages. Configuration comes from the environment (`.env.example` lists every setting).
- **Tests don't touch the internet.** Use fake servers on 127.0.0.1 (tests switch `netguard.AllowNonPublic` on) or a
  network namespace.
- Keep the docs true: if behaviour or a setting changes, update `README.md`, `docs/CRAWLER_NODES.md` and
  `.env.example` in the same pull request.

## Reporting bugs

Open an issue with the file and line, how to reproduce it, what you expected, what happened, and a suggested fix if
you have one. For security problems, email [cl0qsearch@gmail.com](mailto:cl0qsearch@gmail.com) instead of opening a
public issue.
