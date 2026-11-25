-- The catalog's audit receipts, numbered by its document's activity sequence,
-- and its content-addressed instance logos. A catalog change writes both in
-- the transaction that writes the document.
CREATE TABLE catalog_activity (
    org text NOT NULL,
    id bigint NOT NULL CHECK (id > 0),
    entry jsonb NOT NULL,
    PRIMARY KEY (org, id)
);

CREATE TABLE catalog_logos (
    org text NOT NULL,
    hash text NOT NULL,
    image bytea NOT NULL,
    PRIMARY KEY (org, hash)
);
