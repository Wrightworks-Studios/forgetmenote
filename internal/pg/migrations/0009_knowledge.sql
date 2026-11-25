-- Each notebook's semantic vectors, as its cache/semantic.bin held them: one
-- set row names the configuration and revision they were embedded under, and
-- one row per passage content hash holds the little-endian float32 vector.
-- version changes on every write, so an engine that last wrote the set adds and
-- removes only the rows that changed, and any other rewrites the set. written is
-- the set version that added a row, so another engine reads only newer rows.
CREATE TABLE knowledge_vector_sets (
    org text NOT NULL,
    notebook text COLLATE "C" NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    config_id text COLLATE "C" NOT NULL,
    revision text COLLATE "C" NOT NULL,
    dimensions integer NOT NULL CHECK (dimensions BETWEEN 1 AND 4096),
    PRIMARY KEY (org, notebook)
);
CREATE TABLE knowledge_vectors (
    org text NOT NULL,
    notebook text COLLATE "C" NOT NULL,
    hash text COLLATE "C" NOT NULL,
    written bigint NOT NULL CHECK (written > 0),
    vector bytea NOT NULL,
    PRIMARY KEY (org, notebook, hash),
    FOREIGN KEY (org, notebook) REFERENCES knowledge_vector_sets ON DELETE CASCADE
);
CREATE INDEX knowledge_vectors_written ON knowledge_vectors (org, notebook, written);
