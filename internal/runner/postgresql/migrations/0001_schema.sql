-- The second storage kind's schema (docs/design/store-postgresql.md,
-- section 3): every table of the sqlite kind by the same name, with the same
-- columns and keys, and the second kind's own rows. Applied by
-- `jpack-runner store migrate` as the owner role, in one transaction, with
-- jpack.migration set for it. It creates no role and stores no setting.
CREATE SCHEMA jpack_runner;
SET LOCAL search_path TO jpack_runner, pg_catalog;

CREATE TABLE metadata (key text PRIMARY KEY, value text NOT NULL);
CREATE TABLE releases (id text PRIMARY KEY, record text NOT NULL);

-- A record's scope column is the record's own scope member, read through
-- json and never jsonb (D5).
CREATE TABLE jobs (
  seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id text NOT NULL,
  release_id text NOT NULL,
  record text NOT NULL,
  scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope', '')) STORED NOT NULL,
  CONSTRAINT jobs_id_key UNIQUE (id),
  CONSTRAINT jobs_release_id_key UNIQUE (release_id),
  CONSTRAINT jobs_id_scope_key UNIQUE (id, scope)
);
CREATE TABLE job_creations (
  release_id text NOT NULL,
  record text NOT NULL,
  scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope', '')) STORED NOT NULL,
  CONSTRAINT job_creations_pkey PRIMARY KEY (release_id)
);
CREATE TABLE job_revisions (
  job_id text NOT NULL,
  revision integer NOT NULL,
  release_id text NOT NULL,
  record text NOT NULL,
  request_digest text NOT NULL DEFAULT '',
  scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope', '')) STORED NOT NULL,
  CONSTRAINT job_revisions_pkey PRIMARY KEY (job_id, revision),
  CONSTRAINT job_revisions_release_id_key UNIQUE (release_id)
);

CREATE TABLE runs (
  seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id text NOT NULL,
  job_id text NOT NULL,
  caller text NOT NULL,
  idem text NOT NULL,
  request_digest text NOT NULL,
  state text NOT NULL,
  record text NOT NULL,
  -- The claim (D3): its token, the attempt claimed, and the lease by the
  -- database's clock.
  claim_id text,
  claim_attempt integer,
  lease_until timestamptz,
  scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope', '')) STORED NOT NULL,
  CONSTRAINT runs_id_key UNIQUE (id),
  CONSTRAINT runs_request_key UNIQUE (job_id, caller, idem),
  CONSTRAINT runs_id_scope_key UNIQUE (id, scope)
);
CREATE INDEX runs_state ON runs (state, seq);
CREATE INDEX runs_job ON runs (job_id, seq);

-- A brief carries its subject's scope, filled from the subject's row in the
-- statement that writes it, and held to it by a key (D5).
CREATE TABLE briefs (
  subject text PRIMARY KEY,
  record bytea NOT NULL,
  job_id text,
  run_id text,
  scope text NOT NULL,
  CONSTRAINT briefs_subject_check CHECK ((job_id IS NULL) <> (run_id IS NULL)),
  CONSTRAINT briefs_job_fkey FOREIGN KEY (job_id, scope) REFERENCES jobs (id, scope),
  CONSTRAINT briefs_run_fkey FOREIGN KEY (run_id, scope) REFERENCES runs (id, scope)
);

CREATE TABLE triggers (
  seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id text NOT NULL,
  job_id text NOT NULL,
  record text NOT NULL,
  key_hash text NOT NULL DEFAULT '',
  observed text NOT NULL DEFAULT '',
  pending text NOT NULL DEFAULT '',
  pending_at text NOT NULL DEFAULT '',
  key_revision integer,
  scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope', '')) STORED NOT NULL,
  CONSTRAINT triggers_id_key UNIQUE (id)
);
CREATE INDEX triggers_job ON triggers (job_id);
-- One trigger per cloud binding, for every scope (D9).
CREATE UNIQUE INDEX triggers_cloud_binding ON triggers ((record::json->'config'->'cloud'->>'connection'), (record::json->'config'->'cloud'->>'job')) WHERE record::json->'config'->>'kind' = 'cloud';
CREATE TABLE trigger_revisions (
  trigger_id text NOT NULL,
  revision integer NOT NULL,
  record text NOT NULL,
  scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope', '')) STORED NOT NULL,
  CONSTRAINT trigger_revisions_pkey PRIMARY KEY (trigger_id, revision)
);

CREATE TABLE occurrences (
  seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id text NOT NULL,
  job_id text NOT NULL,
  trigger_id text NOT NULL,
  identity text NOT NULL,
  payload_digest text NOT NULL,
  state text NOT NULL,
  record text NOT NULL,
  scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope', '')) STORED NOT NULL,
  CONSTRAINT occurrences_id_key UNIQUE (id),
  CONSTRAINT occurrences_identity_key UNIQUE (trigger_id, identity),
  CONSTRAINT occurrences_id_scope_key UNIQUE (id, scope)
);
CREATE INDEX occurrences_state ON occurrences (state, seq);
CREATE INDEX occurrences_job ON occurrences (job_id, seq);
CREATE TABLE occurrence_keys (
  occurrence_id text PRIMARY KEY,
  key_hash text NOT NULL,
  scope text NOT NULL,
  CONSTRAINT occurrence_keys_occurrence_fkey FOREIGN KEY (occurrence_id, scope) REFERENCES occurrences (id, scope)
);

-- The journal: its sequence is taken from journal_head, under the lock every
-- writer takes first (D13).
CREATE TABLE events (
  sequence bigint PRIMARY KEY,
  entry_version text NOT NULL,
  kind text NOT NULL,
  at text NOT NULL,
  by_actor text NOT NULL,
  concerns text NOT NULL,
  revision integer,
  from_state text,
  to_state text,
  reason text NOT NULL DEFAULT '',
  detail text NOT NULL DEFAULT '{}',
  job_id text NOT NULL DEFAULT '',
  release_id text NOT NULL DEFAULT '',
  -- A refusal's key joins its parts with NUL (events.go), which text does
  -- not hold.
  refusal bytea NOT NULL DEFAULT '\x',
  scope text GENERATED ALWAYS AS (coalesce(detail::json->>'scope', '')) STORED NOT NULL
);
CREATE INDEX events_job ON events (job_id, sequence);
CREATE INDEX events_release ON events (release_id, sequence);
CREATE INDEX events_refusal ON events (refusal, sequence) WHERE refusal <> '\x';
CREATE INDEX events_kind ON events (kind, sequence);

CREATE TABLE run_chain (
  sequence bigint PRIMARY KEY CHECK (sequence > 0),
  run text NOT NULL,
  line bytea NOT NULL,
  CONSTRAINT run_chain_run_key UNIQUE (run)
);

-- One row each: the journal's last sequence (D13) and the chain's (D14).
CREATE TABLE journal_head (last bigint NOT NULL);
CREATE TABLE chain_head (last bigint NOT NULL);
-- The rules held for the installation (D9).
CREATE TABLE counts (name text PRIMARY KEY, value bigint NOT NULL CHECK (value >= 0));
-- The identities of each revision's release, one row for each, so that a
-- conflict is counted over the revisions as the sqlite kind counts it.
CREATE TABLE pack_identities (release_id text PRIMARY KEY, pack text NOT NULL, version text NOT NULL, digest text NOT NULL);
CREATE INDEX pack_identities_pack ON pack_identities (pack, version);
CREATE TABLE dialect_identities (release_id text PRIMARY KEY, dialect text NOT NULL, version text NOT NULL, digest text NOT NULL);
CREATE INDEX dialect_identities_dialect ON dialect_identities (dialect, version);

CREATE TABLE schema_migrations (
  version text PRIMARY KEY,
  digest text NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now(),
  request_login text NOT NULL DEFAULT '',
  worker_login text NOT NULL DEFAULT ''
);

INSERT INTO journal_head VALUES (0);
INSERT INTO chain_head VALUES (0);
INSERT INTO counts VALUES ('triggers', 0), ('active-runs', 0), ('pending-occurrences', 0);
-- The schema row is the migration's (D15): the latest the set knows.
INSERT INTO metadata VALUES ('schema', '6');

-- Row security, enabled and forced on every table (section 6). The owner
-- reaches rows only while its transaction sets jpack.migration, which the
-- command sets in each file's transaction; neither run-time login is the
-- owner.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['metadata', 'releases', 'jobs', 'job_creations', 'job_revisions', 'runs', 'briefs', 'triggers', 'trigger_revisions', 'occurrences', 'occurrence_keys', 'events', 'run_chain', 'journal_head', 'chain_head', 'counts', 'pack_identities', 'dialect_identities', 'schema_migrations'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY owner_migration ON %I TO CURRENT_ROLE USING (current_setting(''jpack.migration'', true) = ''on'') WITH CHECK (current_setting(''jpack.migration'', true) = ''on'')', t);
  END LOOP;
END $$;
