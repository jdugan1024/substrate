-- Open Brain: self-hosted PostgreSQL schema
-- Run this against a non-superuser role (see notes at bottom).

-- Extensions
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "vector";

-- ---------------------------------------------------------------------------
-- Users
-- Each user is identified by their OIDC subject from Authelia.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS mcp_users (
    id           UUID        DEFAULT gen_random_uuid() PRIMARY KEY,
    name         TEXT        NOT NULL,
    oidc_subject TEXT        NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Thoughts
-- Identical to the original schema, with user_id added.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS thoughts (
    id         UUID        DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id    UUID        NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
    content    TEXT        NOT NULL,
    embedding  VECTOR(1536),
    metadata   JSONB       DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS thoughts_embedding_idx  ON thoughts USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS thoughts_metadata_idx   ON thoughts USING gin  (metadata);
CREATE INDEX IF NOT EXISTS thoughts_created_at_idx ON thoughts (created_at DESC);
CREATE INDEX IF NOT EXISTS thoughts_user_id_idx    ON thoughts (user_id);

CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS thoughts_updated_at ON thoughts;
CREATE TRIGGER thoughts_updated_at
    BEFORE UPDATE ON thoughts
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- Row Level Security
--
-- The Go server calls `SET LOCAL app.current_user_id = '<uuid>'` at the start
-- of every transaction. These policies enforce isolation automatically.
--
-- IMPORTANT: The DATABASE_URL role must NOT be a superuser — superusers bypass
-- RLS. Create a dedicated role:
--
--   CREATE ROLE app_user LOGIN PASSWORD 'changeme';
--   GRANT CONNECT ON DATABASE yourdb TO app_user;
--   GRANT USAGE ON SCHEMA public TO app_user;
--   GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_user;
--   GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO app_user;
--   ALTER DEFAULT PRIVILEGES IN SCHEMA public
--     GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_user;
-- ---------------------------------------------------------------------------
ALTER TABLE thoughts ENABLE ROW LEVEL SECURITY;

CREATE POLICY thoughts_user_isolation ON thoughts
    FOR ALL
    USING (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    );

-- ---------------------------------------------------------------------------
-- API Tokens
-- Long-lived personal access tokens for headless clients (e.g. the capture
-- daemon). Looked up by SHA-256 hash at auth time, before any user context
-- exists, so this table has NO row-level security (like mcp_users). Handlers
-- that create/list/revoke tokens filter by user_id explicitly.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_tokens (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens (user_id);

-- mcp_users is readable by the app role but not subject to per-user RLS
-- (the server needs to look up any user by oidc_subject at auth time).

-- ---------------------------------------------------------------------------
-- Semantic search function
--
-- SECURITY INVOKER (the default) means RLS on `thoughts` applies automatically
-- because the function runs in the caller's security context.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION match_thoughts(
    query_embedding VECTOR(1536),
    match_threshold FLOAT DEFAULT 0.5,
    match_count     INT   DEFAULT 10
)
RETURNS TABLE (
    id         UUID,
    content    TEXT,
    metadata   JSONB,
    similarity FLOAT,
    created_at TIMESTAMPTZ
)
LANGUAGE plpgsql
SECURITY INVOKER
AS $$
BEGIN
    RETURN QUERY
    SELECT
        t.id,
        t.content,
        t.metadata,
        1 - (t.embedding <=> query_embedding) AS similarity,
        t.created_at
    FROM thoughts t
    WHERE 1 - (t.embedding <=> query_embedding) > match_threshold
    ORDER BY t.embedding <=> query_embedding
    LIMIT match_count;
END;
$$;

-- ---------------------------------------------------------------------------
-- Canonical entries table
--
-- All captured records — thoughts, contacts, maintenance tasks, job applications,
-- etc. — are stored here as typed JSONB payloads validated against JSON Schemas.
-- The record_type field (e.g. 'crm.contact', 'note.thought') determines which
-- schema applies. source tracks the capture path ('web', 'mcp', 'migrated').
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS entries (
    id              UUID             PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID             NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
    record_type     TEXT             NOT NULL,
    schema_version  TEXT             NOT NULL DEFAULT '1.0.0',
    source          TEXT             NOT NULL DEFAULT 'web',
    confidence      DOUBLE PRECISION,
    failure_mode    TEXT,            -- 'low_confidence' | 'validation_failure' | NULL
    content_text    TEXT             NOT NULL,
    payload         JSONB            NOT NULL,
    tags            JSONB            NOT NULL DEFAULT '[]'::jsonb,
    entities        JSONB            NOT NULL DEFAULT '{}'::jsonb,
    embedding       VECTOR(1536),
    created_at      TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ      NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_entries_user_id       ON entries (user_id);
CREATE INDEX IF NOT EXISTS idx_entries_record_type   ON entries (user_id, record_type);
CREATE INDEX IF NOT EXISTS idx_entries_created_at    ON entries (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_entries_payload_gin   ON entries USING gin (payload);
CREATE INDEX IF NOT EXISTS idx_entries_tags_gin      ON entries USING gin (tags);
CREATE INDEX IF NOT EXISTS idx_entries_entities_gin  ON entries USING gin (entities);
CREATE INDEX IF NOT EXISTS idx_entries_embedding     ON entries USING hnsw (embedding vector_cosine_ops);

ALTER TABLE entries ENABLE ROW LEVEL SECURITY;

CREATE POLICY entries_user_isolation ON entries
    FOR ALL
    USING (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    );

DROP TRIGGER IF EXISTS entries_updated_at ON entries;
CREATE TRIGGER entries_updated_at
    BEFORE UPDATE ON entries
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- Captured Sessions
-- Tracks live-captured LLM conversations per (user, tool, session_id).
-- chunked_msg_count = how many transcript messages have been folded into
-- emitted raw chunk entries (dedup high-water mark). summary_entry_id points
-- at the single upserted conversation.summary entry for the session.
-- Separate from conversation_imports (batch Claude Desktop export path).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS captured_sessions (
    user_id            UUID        NOT NULL REFERENCES mcp_users(id) ON DELETE CASCADE,
    tool               TEXT        NOT NULL,
    session_id         TEXT        NOT NULL,
    summary_entry_id   UUID        REFERENCES entries(id) ON DELETE SET NULL,
    chunked_msg_count  INT         NOT NULL DEFAULT 0,
    message_count      INT         NOT NULL DEFAULT 0,
    session_started_at TIMESTAMPTZ,
    session_ended_at   TIMESTAMPTZ,
    last_summarized_at TIMESTAMPTZ,
    last_ingested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, tool, session_id)
);

ALTER TABLE captured_sessions ENABLE ROW LEVEL SECURITY;

CREATE POLICY captured_sessions_user_isolation ON captured_sessions
    FOR ALL
    USING (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.current_user_id', true) IS NOT NULL
        AND user_id = current_setting('app.current_user_id', true)::uuid
    );
