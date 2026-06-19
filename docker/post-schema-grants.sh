#!/bin/bash
# Safety-net grants after all schema files have been applied.
# ALTER DEFAULT PRIVILEGES covers future objects, but these catch anything
# created by the init scripts themselves.

set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_user;
    GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO app_user;
    GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_user;

    -- Enrichment worker only needs to read and update link entries.
    GRANT SELECT, UPDATE ON entries TO enrichment_worker;
EOSQL
