# AGENTS.md

Guidance for AI coding agents working in this repository. This is the single
source of truth; `CLAUDE.md` just imports it.

## Overview

Engram is a self-hosted **Go MCP server for persistent AI memory**, backed by
PostgreSQL + pgvector. It gives any MCP-compatible client (Claude Desktop,
Claude Code, etc.) a personal memory layer: capture freeform thoughts with
automatic metadata extraction and embeddings, recall them by semantic search,
and store structured data through extensions. Multi-user, with per-user
isolation enforced by PostgreSQL Row Level Security (RLS). Derived from
[Open Brain](https://github.com/NateBJones-Projects/OB1).

> The Go module is named `open-brain-go` for historical reasons. This is
> intentional — do not "fix" it.

## Build, test, run

```bash
go build ./...        # build everything
go test ./...         # run the full test suite
gofmt -l .            # check formatting (should print nothing)
go vet ./...          # static checks
```

**Guardrail:** run `go test ./...`, `gofmt`, and `go vet` and confirm they pass
before considering any change complete. Tests live next to the code they cover
(`*_test.go`); add tests alongside new logic.

Run the local stack (secrets injected via SOPS):

```bash
sops exec-env secrets/engram.env 'docker compose up -d --build'
docker compose logs -f
```

**Guardrail:** the `engram` stack above is **live**. For verifying changes, use
the isolated test stack instead, and never decrypt secrets or touch the live
stack unless the user explicitly asks:

```bash
# isolated smoke-test stack — distinct project name, never touches live data
sops exec-env secrets/engram.env \
  'docker compose -p engram-test -f docker-compose.test.yml up -d --build'
# tear down (wipes the test DB volume)
docker compose -p engram-test -f docker-compose.test.yml down -v
```

The server listens on `:8080` (Streamable HTTP MCP transport).

## Architecture

- **`main.go` + root handlers** (`web.go`, `web_auth.go`, `pwa.go`,
  `api_tokens.go`, `ingest_handler.go`) — process entrypoint, HTTP/MCP server,
  OIDC auth (Authelia at `auth.x1024.net`), web UI/PWA, and API-token issuance
  for MCP clients.
- **`brain/`** — application wiring (`app.go`), the extension `registry`,
  enrichment (`extractor.go`, `fetcher.go`, `validator.go`), and OIDC helpers.
  - `brain/repository/` — database access (pgx). Keep SQL here.
  - `brain/service/` — business logic (ingest, entries), built on repositories.
- **`core/`** — memory primitives: `thoughts.go`, `add_item.go`, `search.go`.
- **`extensions/<name>/`** — self-contained domain modules (household,
  maintenance, calendar, meals, crm, jobhunt). Each owns its `schema.sql` and is
  wired in through the registry.
- **`cmd/engram-capture/`** — a separate daemon that watches and parses Claude
  Code and Codex transcripts and ingests them into Engram.

**Data layer:** PostgreSQL 17 + pgvector. Embeddings and metadata extraction go
through OpenRouter. The schema is auto-applied on first boot by the init scripts
in `docker/` (core `schema.sql` + each extension's `schema.sql`).

**Layering:** handlers → service → repository. Don't reach past a layer (e.g.
no raw SQL in handlers).

## Conventions & guardrails

- **RLS + extensions:** new features must follow the multi-user RLS model and
  the per-extension `schema.sql` + registry pattern. Do not add ad-hoc tables
  outside this structure.
- **Commits:** use conventional commits matching existing history —
  `feat:`, `fix:`, `chore:`, `docs:`.
- **Keep DB access in `repository/`**; put logic in `service/`.

## Secrets & deploy

Secrets are managed with **SOPS + age**. The encrypted file is
`secrets/engram.env`; the age key lives at `~/.config/sops/age/keys.txt`. Never
commit plaintext secrets, and never print decrypted secret values.

## Gotchas

- Module name is `open-brain-go` (see Overview) — don't rename it.
- Schema init scripts run **only on a fresh DB volume**. Changes to `schema.sql`
  or an extension's `schema.sql` won't apply to an existing database. Test
  schema changes against a wiped test stack
  (`docker compose -p engram-test ... down -v` then `up`).
