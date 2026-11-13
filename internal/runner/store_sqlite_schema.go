package runner

// The sqlite kind's tables beside its own (store_sqlite.go): the triggers and
// occurrences, the chain of runs, and the jobs' creations and revisions,
// which a migration fills from the jobs an earlier Runner kept. Their bytes
// are the ones the Runner wrote before they moved here.

// triggerSchema is the triggers' and occurrences' tables
// (store_sqlite_triggers.go).
const triggerSchema = `
 CREATE TABLE IF NOT EXISTS triggers(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL,record TEXT NOT NULL,key_hash TEXT NOT NULL DEFAULT '',observed TEXT NOT NULL DEFAULT '',pending TEXT NOT NULL DEFAULT '',pending_at TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS trigger_revisions(trigger_id TEXT NOT NULL,revision INTEGER NOT NULL,record TEXT NOT NULL,PRIMARY KEY(trigger_id,revision));
 CREATE TABLE IF NOT EXISTS occurrences(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL,trigger_id TEXT NOT NULL,identity TEXT NOT NULL,payload_digest TEXT NOT NULL,state TEXT NOT NULL,record TEXT NOT NULL,UNIQUE(trigger_id,identity));
 CREATE TABLE IF NOT EXISTS occurrence_keys(occurrence_id TEXT PRIMARY KEY,key_hash TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS occurrences_state ON occurrences(state,seq);
 CREATE INDEX IF NOT EXISTS triggers_job ON triggers(job_id);
 CREATE INDEX IF NOT EXISTS occurrences_job ON occurrences(job_id,seq);
 CREATE UNIQUE INDEX IF NOT EXISTS triggers_cloud_binding ON triggers(json_extract(record,'$.config.cloud.connection'),json_extract(record,'$.config.cloud.job')) WHERE json_extract(record,'$.config.kind')='cloud';`

// runChainSchema is the installation's chain of runs (run_chain.go).
const runChainSchema = `CREATE TABLE IF NOT EXISTS run_chain (sequence INTEGER PRIMARY KEY CHECK (sequence > 0), run TEXT UNIQUE NOT NULL, line BLOB NOT NULL);`

// jobRevisionSchema is the jobs' creations and revisions
// (store_sqlite_jobs.go), filled from the jobs held when a store is raised to
// storeSchema.
const jobRevisionSchema = `CREATE TABLE job_creations (release_id TEXT PRIMARY KEY, record TEXT NOT NULL);
 CREATE TABLE job_revisions (job_id TEXT NOT NULL, revision INTEGER NOT NULL, release_id TEXT UNIQUE NOT NULL, record TEXT NOT NULL, request_digest TEXT NOT NULL DEFAULT '', PRIMARY KEY(job_id,revision));
 INSERT INTO job_creations SELECT release_id,record FROM jobs;
 INSERT INTO job_revisions(job_id,revision,release_id,record) SELECT id,json_extract(record,'$.revision'),release_id,record FROM jobs;`
