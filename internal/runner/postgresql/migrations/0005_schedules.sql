-- Cron and shared-interval schedules (docs/design/schedules.md, section 6).
-- A trigger's schedule is a member of its record, so the file adds no
-- column and grants no privilege. It raises the store's schema to the one
-- that holds such a schedule, as the migration writes the latest schema
-- (D15): an earlier Runner refuses the store, which it would misread.
SET LOCAL search_path TO jpack_runner, pg_catalog;

UPDATE metadata SET value = '7' WHERE key = 'schema';
