-- The run-time logins' privileges and row policies
-- (docs/design/store-postgresql.md, sections 6 and 7; D6). {{request}} and
-- {{worker}} are filled by `jpack-runner store migrate` with the two logins
-- the operator names, as quoted identifiers; the file's row in
-- schema_migrations records the names it was applied with. Neither login is
-- granted a privilege on a sequence, DELETE, TRUNCATE, REFERENCES or TRIGGER.
SET LOCAL search_path TO jpack_runner, pg_catalog;

GRANT USAGE ON SCHEMA jpack_runner TO {{request}}, {{worker}};

-- Unscoped tables: every row, for each login that holds a privilege on it.
GRANT SELECT, INSERT ON releases TO {{request}};
GRANT SELECT ON releases TO {{worker}};
CREATE POLICY request_rows ON releases TO {{request}} USING (true) WITH CHECK (true);
CREATE POLICY worker_rows ON releases TO {{worker}} USING (true) WITH CHECK (true);

-- metadata: both read every row; the request login writes only the profile
-- rows, the worker login inserts only the store's identity, and the schema
-- row is the migration's (D15).
GRANT SELECT, INSERT, UPDATE ON metadata TO {{request}};
GRANT SELECT, INSERT ON metadata TO {{worker}};
CREATE POLICY request_read ON metadata FOR SELECT TO {{request}} USING (true);
CREATE POLICY request_profiles_insert ON metadata FOR INSERT TO {{request}} WITH CHECK (key IN ('registered-input-profiles-v1', 'selected-input-profiles-v1'));
CREATE POLICY request_profiles_update ON metadata FOR UPDATE TO {{request}} USING (key IN ('registered-input-profiles-v1', 'selected-input-profiles-v1')) WITH CHECK (key IN ('registered-input-profiles-v1', 'selected-input-profiles-v1'));
CREATE POLICY worker_read ON metadata FOR SELECT TO {{worker}} USING (true);
CREATE POLICY worker_identity_insert ON metadata FOR INSERT TO {{worker}} WITH CHECK (key IN ('workspace', 'owner', 'inputRoot'));

GRANT SELECT ON run_chain TO {{request}};
GRANT SELECT, INSERT ON run_chain TO {{worker}};
CREATE POLICY request_rows ON run_chain TO {{request}} USING (true) WITH CHECK (true);
CREATE POLICY worker_rows ON run_chain TO {{worker}} USING (true) WITH CHECK (true);

GRANT SELECT, INSERT ON pack_identities, dialect_identities TO {{request}};
GRANT SELECT ON pack_identities, dialect_identities TO {{worker}};
CREATE POLICY request_rows ON pack_identities TO {{request}} USING (true) WITH CHECK (true);
CREATE POLICY worker_rows ON pack_identities TO {{worker}} USING (true) WITH CHECK (true);
CREATE POLICY request_rows ON dialect_identities TO {{request}} USING (true) WITH CHECK (true);
CREATE POLICY worker_rows ON dialect_identities TO {{worker}} USING (true) WITH CHECK (true);

-- UPDATE on journal_head is what LOCK TABLE ... IN EXCLUSIVE MODE requires
-- (D13).
GRANT SELECT, UPDATE ON journal_head, counts TO {{request}}, {{worker}};
CREATE POLICY request_rows ON journal_head TO {{request}} USING (true) WITH CHECK (true);
CREATE POLICY worker_rows ON journal_head TO {{worker}} USING (true) WITH CHECK (true);
CREATE POLICY request_rows ON counts TO {{request}} USING (true) WITH CHECK (true);
CREATE POLICY worker_rows ON counts TO {{worker}} USING (true) WITH CHECK (true);

GRANT SELECT, UPDATE ON chain_head TO {{worker}};
CREATE POLICY worker_rows ON chain_head TO {{worker}} USING (true) WITH CHECK (true);

GRANT SELECT ON schema_migrations TO {{request}}, {{worker}};
CREATE POLICY request_rows ON schema_migrations FOR SELECT TO {{request}} USING (true);
CREATE POLICY worker_rows ON schema_migrations FOR SELECT TO {{worker}} USING (true);

-- Scoped tables: the request login reaches the rows of the scope its
-- transaction sets ('s:' and the scope identifier; 's:' alone for none), and
-- a transaction that set none reaches no row; the worker login does the work
-- for every scope (D7).
GRANT SELECT, INSERT, UPDATE ON jobs, triggers, occurrences, runs, briefs TO {{request}};
GRANT SELECT, INSERT ON job_creations, job_revisions, occurrence_keys TO {{request}};
GRANT INSERT ON trigger_revisions TO {{request}};
GRANT SELECT ON jobs, job_creations, job_revisions TO {{worker}};
GRANT SELECT, UPDATE ON triggers TO {{worker}};
GRANT SELECT, INSERT, UPDATE ON occurrences, runs TO {{worker}};
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['jobs', 'job_creations', 'job_revisions', 'triggers', 'trigger_revisions', 'occurrences', 'occurrence_keys', 'runs', 'briefs'] LOOP
    EXECUTE format('CREATE POLICY request_scope ON %I TO {{request}} USING (current_setting(''jpack.scope'', true) = ''s:'' || scope) WITH CHECK (current_setting(''jpack.scope'', true) = ''s:'' || scope)', t);
  END LOOP;
  FOREACH t IN ARRAY ARRAY['jobs', 'job_creations', 'job_revisions', 'triggers', 'occurrences', 'runs'] LOOP
    EXECUTE format('CREATE POLICY worker_rows ON %I TO {{worker}} USING (true) WITH CHECK (true)', t);
  END LOOP;
END $$;

-- The journal: a request reads its scope's entries and the none scope's
-- entries that name no job (the Runner's own, previews, refused creations),
-- and writes only its scope's.
GRANT SELECT, INSERT ON events TO {{request}}, {{worker}};
CREATE POLICY request_read ON events FOR SELECT TO {{request}} USING (current_setting('jpack.scope', true) = 's:' || scope OR (starts_with(current_setting('jpack.scope', true), 's:') AND scope = '' AND job_id = ''));
CREATE POLICY request_write ON events FOR INSERT TO {{request}} WITH CHECK (current_setting('jpack.scope', true) = 's:' || scope);
CREATE POLICY worker_rows ON events TO {{worker}} USING (true) WITH CHECK (true);
