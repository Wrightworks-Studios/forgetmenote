-- Quicknotes capture records, one row per capture under its account, and each
-- account's capture settings. A capture change writes only over the version it
-- read and a settings change only over the generation it read, so gateways
-- never lose each other's updates. Keys compare bytewise, as the cursor of the
-- background scan does.
CREATE TABLE captures (
    org text NOT NULL,
    account text COLLATE "C" NOT NULL,
    id text COLLATE "C" NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    created_at text COLLATE "C" NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (org, account, id)
);

CREATE INDEX captures_recent ON captures (org, account, created_at DESC);

CREATE TABLE capture_settings (
    org text NOT NULL,
    account text COLLATE "C" NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    settings jsonb NOT NULL,
    PRIMARY KEY (org, account)
);
