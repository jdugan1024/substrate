# Live Conversation Capture — Handoff

**Date:** 2026-06-13
**Status:** Part 1 (server) merged to `main`; **not yet deployed**. Part 2 (daemon) not started.

This document covers two outstanding tasks:

1. **Deploy runbook** — ship the merged server-side work to the live `engram` stack.
2. **Part 2 plan outline** — the capture daemon that actually feeds `POST /ingest`.

## Context / what already exists

- **Design spec:** [`docs/superpowers/specs/2026-06-13-live-conversation-capture-design.md`](../specs/2026-06-13-live-conversation-capture-design.md)
- **Server implementation plan (Part 1 of 2):** [`docs/superpowers/plans/2026-06-13-live-conversation-capture-server.md`](2026-06-13-live-conversation-capture-server.md)
- **Merge commit:** `b9aa8fc` ("Merge branch 'live-capture-server': PAT auth + POST /ingest")

What landed on `main` (Part 1):

- `api_tokens` + `captured_sessions` tables (`schema.sql`).
- PAT auth (`engram_pat_` prefix, SHA-256 hashed) — branch in `authMiddleware`, repository in `brain/repository/api_token.go`.
- `POST /ingest` endpoint (`ingest_handler.go`) → `IngestService` (`brain/service/ingest.go`): full-transcript batches, dedup via integer `chunked_msg_count`, append-only `conversation.chunk` entries + one throttled, upserted `conversation.summary` entry.
- Token-management web UI: `web/tokens.html`, routes in `web.go`, handlers in `api_tokens.go`.
- Smoke-test harness: `docker-compose.test.yml` (isolated `engram-test` project).

**Smoke test status:** passed end-to-end against an isolated stack on 2026-06-13 (chunk + summary written, idempotent re-POST = 0 new chunks, revoked PAT → 401). The live stack was never touched.

---

## Task 1 — Deploy runbook (Part 1 to production)

### Why it isn't automatic

The compose `db` service mounts `schema.sql` and the extension schemas into
`/docker-entrypoint-initdb.d/`, **but those scripts only run when the Postgres
data volume is empty** (first boot). The live DB volume (`engram_pgdata`)
already exists, so a plain `docker compose up -d --build` rebuilds the server
binary but will **not** create the new `api_tokens` / `captured_sessions`
tables. They must be applied manually. Both are `CREATE TABLE IF NOT EXISTS`
(and the policies/indexes are `IF NOT EXISTS`), so re-applying `schema.sql` is
idempotent and safe.

### Pre-flight

- Confirm you're on `main` at or after `b9aa8fc`: `git -C /home/jdugan/engram log --oneline -1`.
- The live stack runs as compose project `engram` (containers `engram`, `engram-db-1`).
- `sops` needs the age key to decrypt `secrets/engram.env`. (During the smoke
  test the age key was **not** available in the agent environment — make sure
  it's present in `~/.config/sops/age/keys.txt` or `$SOPS_AGE_KEY_FILE` before
  running the `sops exec-env` steps below. If `sops` fails to decrypt, you can
  read live env values from the running container, e.g.
  `docker exec engram printenv OPENROUTER_API_KEY`.)

### Steps

1. **Back up the database** (always, before a schema change):

   ```bash
   docker exec engram-db-1 pg_dump -U postgres openbrain \
     | gzip > ~/engram-backup-$(date +%Y%m%d-%H%M%S).sql.gz
   ```

2. **Apply the schema** to the running DB. `psql` is not on the host; pipe
   `schema.sql` into the db container as the superuser:

   ```bash
   docker exec -i engram-db-1 psql -U postgres -d openbrain -v ON_ERROR_STOP=1 \
     < /home/jdugan/engram/schema.sql
   ```

   Expect `CREATE TABLE` / `CREATE INDEX` / `CREATE POLICY` (or `NOTICE … already
   exists, skipping`). No `ERROR` lines.

3. **Grant the app role on the new tables.** `init-db.sh`'s
   `ALTER DEFAULT PRIVILEGES` only covers objects created by the role that ran
   it; tables created later via the superuser need an explicit grant (the
   smoke-test stack got this for free from `post-schema-grants.sh` on first
   boot). Run:

   ```bash
   docker exec -i engram-db-1 psql -U postgres -d openbrain -v ON_ERROR_STOP=1 <<'SQL'
   GRANT SELECT, INSERT, UPDATE, DELETE ON api_tokens, captured_sessions TO app_user;
   GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_user;
   SQL
   ```

   > Verify: `docker exec engram-db-1 psql -U app_user -d openbrain -c '\dp api_tokens'`
   > should show `app_user` with `arwd` privileges.

4. **Rebuild and restart the server** from the live stack dir/project:

   ```bash
   cd /home/jdugan/engram
   sops exec-env secrets/engram.env 'docker compose up -d --build server'
   docker compose logs --tail=20 server   # expect "listening on :8080"
   ```

5. **Create your real PAT** via the web UI: open
   `https://engram.x1024.net/tokens.html`, log in, create a token (e.g.
   "laptop"), copy the plaintext (shown once).

6. **Production verification** (mirrors the smoke test, against real data — use a
   throwaway `session_id`):

   ```bash
   export ENGRAM_PAT='engram_pat_...'
   curl -sS -X POST https://engram.x1024.net/ingest \
     -H "Authorization: Bearer $ENGRAM_PAT" -H "Content-Type: application/json" \
     -d '{"tool":"claude-code","session_id":"deploy-check-1","title":"deploy check",
          "messages":[{"role":"human","text":"ping","msg_id":"m1"},
                      {"role":"assistant","text":"pong","msg_id":"m2"}],
          "session_ended":true}'
   ```

   Expect `{"chunks_created":1,"summarized":true,"message_count":2}`. Then confirm
   it surfaces via the Engram MCP `search` tool / browse page, and (optionally)
   delete the `deploy-check-1` entries.

### Rollback

- The new tables are additive and unused by existing code paths; leaving them in
  place is harmless. To roll back the **server binary**, redeploy the previous
  image (`git checkout <prev-sha> -- . && docker compose up -d --build server`,
  or retag).
- To drop the tables (only if truly needed):
  `DROP TABLE IF EXISTS captured_sessions; DROP TABLE IF EXISTS api_tokens;`
- Restore data from the Step 1 backup if necessary:
  `gunzip -c <backup>.sql.gz | docker exec -i engram-db-1 psql -U postgres -d openbrain`.

---

## Task 2 — Part 2 plan outline (the capture daemon)

A per-machine background process that watches local transcript files from
multiple AI tools and ships normalized transcripts to `POST /ingest`. The server
is authoritative for dedup, chunking, and summarization, so the daemon stays
thin: parse → normalize → trim → POST.

### Suggested shape

- **Language:** Go (reuse `IngestBatch`/`IngestMessage` wire types; could live in
  this repo under `cmd/engram-capture/` or a sibling module).
- **Config:** PAT (`ENGRAM_PAT`), server URL, per-tool transcript roots, sweep
  interval (default ~30s; spec allows 5–10 min lag), trim budget.

### Tasks to flesh out when writing the full plan

1. **Tool source map + parsers** — one parser per tool, each emitting the common
   normalized `[]IngestMessage` (`role` ∈ {human, assistant}, `text`, `ts`,
   `msg_id`) plus batch metadata (`tool`, `session_id`, `title`, `project`):
   - **Claude Code** — JSONL transcripts under `~/.claude/projects/<slug>/*.jsonl`
     (this repo's own session lives there). `session_id` = file UUID.
   - **Claude Desktop** — local conversation store / export. (Note existing
     `conversation_imports` batch path in
     [`2026-05-11-conversation-import-design.md`](../specs/2026-05-11-conversation-import-design.md)
     — keep the live `captured_sessions` path separate, as the schema comment
     states.)
   - **Codex** — session/rollout files (locate on disk).
   - **Zed** — assistant/agent conversation store.
   - **Pi** (https://pi.dev/) — locate local transcript store.
2. **Watcher** — `fsnotify` on the per-tool roots with a debounced sweep;
   fall back to periodic full scan for tools that rewrite files atomically.
3. **Session state / dedup** — the server dedups via `chunked_msg_count`, so the
   daemon may resend the full trimmed transcript each sweep. Keep a small local
   cursor (last-sent message count + content hash per `session_id`) only to skip
   no-op POSTs and reduce bandwidth.
4. **Trimming + placeholders** — trim long/低-value content (tool spam, large
   pastes) before sending, leaving placeholders; design so the trimmed parts can
   be summarized later (spec requirement). Keep within a configurable byte/token
   budget per batch.
5. **`session_ended` detection (ended-by-age)** — mark a session ended when its
   file has been idle longer than a threshold, so the server flushes the held
   partial tail and does a final summary.
6. **Backfill / catch-up** — `--backfill` walks historical transcripts through
   the same pipeline (server dedup makes it re-runnable); `--dry-run` reports how
   many sessions/messages/estimated cost without POSTing.
7. **Resilience** — retry/backoff on 5xx, stop on 401 (revoked/invalid PAT),
   structured logging, single-instance lock.
8. **Packaging** — systemd user unit / launchd agent for "always running."

### Open questions to resolve before/while planning

- Exact on-disk locations + formats for Codex, Zed, and Pi (needs investigation
  on the actual machine).
- Where the daemon code lives (this repo vs. separate).
- Trim policy specifics (what counts as low-value; placeholder format).
- Local cursor store location/format.

---

## Quick status checklist

- [x] Part 1 implemented, reviewed, smoke-tested, merged to `main` (`b9aa8fc`).
- [ ] **Task 1:** deploy Part 1 to production (runbook above).
- [ ] **Task 2:** write the full Part 2 daemon plan, then implement it.
