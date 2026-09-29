-- Only row level security is switched back off. The grants to anon and
-- authenticated are deliberately not restored: handing those roles the
-- credential tables again is never the intended state.
ALTER TABLE users DISABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens DISABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys DISABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        ALTER TABLE schema_migrations DISABLE ROW LEVEL SECURITY;
    END IF;
END
$$;
