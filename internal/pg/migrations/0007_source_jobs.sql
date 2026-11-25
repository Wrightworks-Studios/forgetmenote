-- Source job manifests. Originals, page checkpoints and results stay in each
-- job's directory, the local form of blob storage; a row publishes its job.
-- reserved holds the bytes admitted for queued or draining work, so every
-- gateway charges it before measuring the job's files.
CREATE TABLE source_jobs (
    org text NOT NULL,
    id text COLLATE "C" NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    reserved bigint NOT NULL CHECK (reserved >= 0),
    manifest bytea NOT NULL,
    PRIMARY KEY (org, id)
);
