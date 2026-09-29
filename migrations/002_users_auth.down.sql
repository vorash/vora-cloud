DROP TRIGGER IF EXISTS update_api_keys_updated_at ON api_keys;
DROP TRIGGER IF EXISTS update_users_updated_at ON users;
DROP FUNCTION IF EXISTS update_updated_at_column();

DROP INDEX IF EXISTS idx_api_keys_user_id;
ALTER TABLE api_keys DROP COLUMN IF EXISTS user_id;
ALTER TABLE api_keys DROP COLUMN IF EXISTS updated_at;

DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;
