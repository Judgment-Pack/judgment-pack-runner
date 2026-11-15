-- The policies the routes held in this release need
-- (docs/design/store-postgresql.md, section 6). {{request}} and {{worker}}
-- are filled as in 0002_grants.sql, and the file's row records the names it
-- was applied with. It grants no privilege.
SET LOCAL search_path TO jpack_runner, pg_catalog;

-- A refused creation names a release, not a job, and is journaled under the
-- none scope with its caller. A transaction held to another scope does not
-- read it: the read policy of 0002_grants.sql admits the none scope's entries
-- that name no job to every scope, and this one, ANDed with it, keeps out of
-- those the refusals, which only a transaction held to the none scope reads.
CREATE POLICY request_refusals_held ON events AS RESTRICTIVE FOR SELECT TO {{request}} USING (current_setting('jpack.scope', true) = 's:' OR scope <> '' OR job_id <> '' OR kind <> 'admission.refused');
