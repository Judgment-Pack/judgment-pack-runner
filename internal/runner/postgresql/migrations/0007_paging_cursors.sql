-- The jobs' cursors between runs (docs/design/paging.md, section 4.2): one
-- row per job and paged source, its record carrying the job's scope
-- identifier, held to it as the job's rows are. {{request}} and {{worker}}
-- are filled as in 0002_grants.sql, and the file's row records the names it
-- was applied with. It raises the store's schema to the one that holds a
-- job keeping a cursor, as the migration writes the latest schema (D15): an
-- earlier Runner refuses the store, which would drop the job's paging member
-- and complete its runs without moving their cursors.
SET LOCAL search_path TO jpack_runner, pg_catalog;

CREATE TABLE paging_cursors (
  job_id text NOT NULL,
  source text NOT NULL,
  record text NOT NULL,
  scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope', '')) STORED NOT NULL,
  CONSTRAINT paging_cursors_pkey PRIMARY KEY (job_id, source)
);
ALTER TABLE paging_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE paging_cursors FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_migration ON paging_cursors TO CURRENT_ROLE USING (current_setting('jpack.migration', true) = 'on') WITH CHECK (current_setting('jpack.migration', true) = 'on');

-- The worker login reads a job's cursors when it prepares an occurrence,
-- and writes one in the transaction that records a run completed, for
-- every scope (D7). The request login reads the rows of the scope its
-- transaction sets, as it reads the job's, and writes none.
GRANT SELECT ON paging_cursors TO {{request}};
GRANT SELECT, INSERT, UPDATE ON paging_cursors TO {{worker}};
CREATE POLICY request_scope ON paging_cursors TO {{request}} USING (current_setting('jpack.scope', true) = 's:' || scope) WITH CHECK (current_setting('jpack.scope', true) = 's:' || scope);
CREATE POLICY worker_rows ON paging_cursors TO {{worker}} USING (true) WITH CHECK (true);

UPDATE metadata SET value = '8' WHERE key = 'schema';
