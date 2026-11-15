-- The profile rows held to the none scope by the database
-- (docs/design/store-postgresql.md, section 7). {{request}} and {{worker}}
-- are filled as in 0002_grants.sql, and the file's row records the names it
-- was applied with. It grants no privilege.
SET LOCAL search_path TO jpack_runner, pg_catalog;

-- The request login writes metadata's profile rows only in a transaction
-- held to the none scope ('s:'): one held to a scope identifier, or that set
-- none, inserts and updates no row. ANDed with the policies of
-- 0002_grants.sql, which admit the profile rows' keys alone.
CREATE POLICY request_profiles_unscoped_insert ON metadata AS RESTRICTIVE FOR INSERT TO {{request}} WITH CHECK (current_setting('jpack.scope', true) = 's:');
CREATE POLICY request_profiles_unscoped_update ON metadata AS RESTRICTIVE FOR UPDATE TO {{request}} USING (current_setting('jpack.scope', true) = 's:') WITH CHECK (current_setting('jpack.scope', true) = 's:');
