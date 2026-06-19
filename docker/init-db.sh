#!/bin/bash
# Runs as the postgres superuser on first boot.
# Creates the app_user role used by the Go server (non-superuser, so RLS applies)
# and the enrichment_worker role used by the background EnrichmentWorker, which
# needs BYPASSRLS to scan link entries across all users (WithAdminTx).

set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    CREATE ROLE app_user LOGIN PASSWORD '${APP_USER_PASSWORD}';
    GRANT CONNECT ON DATABASE ${POSTGRES_DB} TO app_user;
    GRANT USAGE ON SCHEMA public TO app_user;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_user;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT EXECUTE ON FUNCTIONS TO app_user;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT USAGE, SELECT ON SEQUENCES TO app_user;

    -- Enrichment worker: least-privilege BYPASSRLS role. It bypasses RLS only
    -- so its cross-user scan can run. The grant on the entries table happens in
    -- post-schema-grants.sh, after schema.sql creates the table.
    CREATE ROLE enrichment_worker LOGIN PASSWORD '${ENRICHMENT_USER_PASSWORD}' BYPASSRLS;
    GRANT CONNECT ON DATABASE ${POSTGRES_DB} TO enrichment_worker;
    GRANT USAGE ON SCHEMA public TO enrichment_worker;
EOSQL
