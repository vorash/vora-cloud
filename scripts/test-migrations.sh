#!/usr/bin/env bash
# Runs the migrations against throwaway PostgreSQL clusters, so a migration is
# tested before it reaches a real database. Needs the PostgreSQL server
# binaries (initdb, pg_ctl) in PG_BIN, plus the `migrate` CLI and psql.
#
#   PG_BIN=/usr/lib/postgresql/17/bin make test-migrations
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

: "${PG_BIN:?set PG_BIN to the directory holding initdb and pg_ctl}"
[ -x "$PG_BIN/initdb" ] || { echo "no initdb in $PG_BIN" >&2; exit 1; }
command -v migrate >/dev/null || { echo "migrate CLI not found" >&2; exit 1; }
command -v psql >/dev/null || { echo "psql not found" >&2; exit 1; }

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok:   $*"; }
expect() { # NAME EXPECTED ACTUAL
  if [ "$3" = "$2" ]; then pass "$1"; else fail "$1: got '$3', want '$2'"; fi
}

work=$(mktemp -d)
trap 'for d in "$work"/pg-*; do [ -d "$d" ] && "$PG_BIN/pg_ctl" -D "$d" -m immediate stop >/dev/null 2>&1; done; rm -rf "$work"' EXIT

port=54400
start_cluster() { # NAME; sets PSQL and URL
  local dir="$work/pg-$1"
  port=$((port + 1))
  "$PG_BIN/initdb" -D "$dir" -U test --auth=trust -E UTF8 >/dev/null 2>&1 || { echo "initdb failed" >&2; exit 1; }
  "$PG_BIN/pg_ctl" -D "$dir" -o "-p $port -c listen_addresses=127.0.0.1 -c unix_socket_directories=" \
    -l "$work/$1.log" -w start >/dev/null 2>&1 || { echo "postgres did not start" >&2; cat "$work/$1.log" >&2; exit 1; }
  PSQL=(psql -h 127.0.0.1 -p "$port" -U test -d postgres -v ON_ERROR_STOP=1 -Atq)
  URL="postgres://test@127.0.0.1:$port/postgres?sslmode=disable"
}
q() { "${PSQL[@]}" -c "$1"; }
app_relations() {
  q "select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace
     where n.nspname = 'public' and c.relkind in ('r', 'i', 'S') and c.relname not like 'schema_migrations%'"
}
priv() { q "select has_table_privilege('$1', 'public.$2', '$3')"; }

echo "== plain PostgreSQL (no Supabase roles)"
start_cluster plain
if migrate -path migrations -database "$URL" up >/dev/null 2>&1; then pass "up applies every migration"; else fail "up failed"; fi
expect "migration version" 7 "$(q 'select version from schema_migrations')"
expect "row level security is on" t "$(q "select relrowsecurity from pg_class where relname = 'users'")"
# Supabase hands every new public table to anon and authenticated, so a table
# without row level security is readable through the Data API.
expect "every public table has row level security" "" "$(q "select string_agg(c.relname, ', ') from pg_class c
  join pg_namespace n on n.oid = c.relnamespace
  where n.nspname = 'public' and c.relkind in ('r', 'p') and not c.relrowsecurity")"
# Projects and deployments: the constraints the service relies on.
q "insert into users (id, name, email, password_hash) values ('00000000-0000-0000-0000-00000000000a', 'a', 'a@example.com', 'x');
   insert into projects (id, owner_id, name, slug, region) values ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-00000000000a', 'Web', 'web', 'eu')" >/dev/null
if q "insert into projects (owner_id, name, slug, region) values ('00000000-0000-0000-0000-00000000000a', 'Web 2', 'web', 'eu')" >/dev/null 2>&1; then fail "a second project got an existing slug"; else pass "slugs are unique across owners"; fi
if q "insert into projects (owner_id, name, slug, region) values ('00000000-0000-0000-0000-00000000000a', 'Bad', 'Bad_Slug', 'eu')" >/dev/null 2>&1; then fail "an invalid slug was accepted"; else pass "invalid slugs are rejected"; fi
if q "insert into deployments (project_id, image, region, status) values ('00000000-0000-0000-0000-0000000000a1', 'i', 'eu', 'bogus')" >/dev/null 2>&1; then fail "an unknown deployment status was accepted"; else pass "unknown deployment statuses are rejected"; fi
q "insert into deployments (project_id, image, region) values ('00000000-0000-0000-0000-0000000000a1', 'i', 'eu');
   insert into project_env_vars (project_id, name, value_encrypted) values ('00000000-0000-0000-0000-0000000000a1', 'A', '\\x00')" >/dev/null
q "delete from projects" >/dev/null
expect "deleting a project deletes its deployments and variables" 0 "$(q 'select (select count(*) from deployments) + (select count(*) from project_env_vars)')"
q "delete from users" >/dev/null
if migrate -path migrations -database "$URL" down -all >/dev/null 2>&1 <<<"y"; then pass "down -all applies every down migration"; else fail "down -all failed"; fi
expect "down leaves no application tables" 0 "$(app_relations)"
if migrate -path migrations -database "$URL" up >/dev/null 2>&1; then pass "up works again after down"; else fail "second up failed"; fi

echo "== Supabase-like (anon and authenticated exist and receive every new table)"
start_cluster supabase
q "create role anon nologin; create role authenticated nologin;
   alter default privileges in schema public grant all on tables to anon, authenticated" >/dev/null
migrate -path migrations -database "$URL" up 2 >/dev/null 2>&1 || fail "up 2 failed"
# This is the state production was in before migration 003.
expect "before 003: anon can read users" t "$(priv anon users SELECT)"
expect "before 003: anon can write api_keys" t "$(priv anon api_keys INSERT)"
if migrate -path migrations -database "$URL" up 1 >/dev/null 2>&1; then pass "003 applies"; else fail "003 failed"; fi
for role in anon authenticated; do
  for table in users api_keys refresh_tokens schema_migrations; do
    expect "$role has no SELECT on $table" f "$(priv $role $table SELECT)"
    expect "$role has no INSERT on $table" f "$(priv $role $table INSERT)"
  done
done
expect "row level security is on for api_keys" t "$(q "select relrowsecurity from pg_class where relname = 'api_keys'")"
expect "the owner still reads the tables" 0 "$(q 'select count(*) from users')"
q "create table later_table (id int)" >/dev/null
expect "a table created later is not handed to anon" f "$(priv anon later_table SELECT)"
if migrate -path migrations -database "$URL" down 1 >/dev/null 2>&1 <<<"y"; then pass "003 down applies"; else fail "003 down failed"; fi
expect "down turns row level security off" f "$(q "select relrowsecurity from pg_class where relname = 'users'")"
expect "down does not hand the tables back to anon" f "$(priv anon users SELECT)"
migrate -path migrations -database "$URL" up >/dev/null 2>&1 || fail "up to the latest migration failed"
for table in projects deployments project_env_vars sessions github_deliveries; do
  expect "anon has no SELECT on $table" f "$(priv anon $table SELECT)"
  expect "authenticated has no INSERT on $table" f "$(priv authenticated $table INSERT)"
done

if [ "$failures" -gt 0 ]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all migration checks passed"
