-- Versioned JSON documents that owner packages store whole, such as the catalog
-- and OAuth state. Version 0 is the empty document a first writer locks.
CREATE TABLE documents (
    org text NOT NULL,
    kind text NOT NULL,
    id text NOT NULL,
    version bigint NOT NULL CHECK (version >= 0),
    body jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org, kind, id)
);
