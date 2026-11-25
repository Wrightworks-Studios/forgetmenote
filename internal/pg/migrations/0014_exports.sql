-- Chunked notebook exports in progress, so any gateway serves the next chunk.
-- id is the digest of the user, notebook and bundle digest the export belongs
-- to; export_chunks holds the bundle in pieces of 256 KiB, numbered from zero.
-- An export past expires is gone.
CREATE TABLE exports (
    org text NOT NULL,
    id text COLLATE "C" NOT NULL,
    sha text COLLATE "C" NOT NULL,
    total bigint NOT NULL CHECK (total >= 0),
    expires timestamptz NOT NULL,
    PRIMARY KEY (org, id)
);
CREATE TABLE export_chunks (
    org text NOT NULL,
    id text COLLATE "C" NOT NULL,
    n integer NOT NULL CHECK (n >= 0),
    data bytea NOT NULL,
    PRIMARY KEY (org, id, n),
    FOREIGN KEY (org, id) REFERENCES exports (org, id) ON DELETE CASCADE
);
