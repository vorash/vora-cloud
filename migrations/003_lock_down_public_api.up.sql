-- Supabase serves the public schema through its REST API to the anon and
-- authenticated roles, and grants them every new table by default. These
-- tables hold password and API key hashes, and the service connects as the
-- table owner (which bypasses row level security), so nothing here may be
-- reachable by those roles.
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;

-- The roles only exist on Supabase, so plain Postgres skips the revokes.
DO $$
DECLARE
    api_role text;
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        ALTER TABLE schema_migrations ENABLE ROW LEVEL SECURITY;
    END IF;

    FOREACH api_role IN ARRAY ARRAY['anon', 'authenticated'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = api_role) THEN
            EXECUTE format('REVOKE ALL ON TABLE users, refresh_tokens, api_keys FROM %I', api_role);
            IF to_regclass('public.schema_migrations') IS NOT NULL THEN
                EXECUTE format('REVOKE ALL ON TABLE schema_migrations FROM %I', api_role);
            END IF;
            -- Tables created by later migrations must not be handed out either.
            EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM %I', api_role);
        END IF;
    END LOOP;
END
$$;
