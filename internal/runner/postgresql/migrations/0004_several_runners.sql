-- Several Runners on one store (docs/design/store-postgresql.md, D17 and
-- D18). It only adds columns, each with a default, and grants no privilege:
-- the run-time logins' table privileges of 0002_grants.sql cover them.
SET LOCAL search_path TO jpack_runner, pg_catalog;

-- A trigger's row version, moved on by every write of the trigger, which a
-- writer expects as it read it (D17).
ALTER TABLE triggers ADD COLUMN version bigint NOT NULL DEFAULT 0;

-- An occurrence's claim (D18): its token, and its lease by the database's
-- clock.
ALTER TABLE occurrences ADD COLUMN claim_id text, ADD COLUMN lease_until timestamptz;
