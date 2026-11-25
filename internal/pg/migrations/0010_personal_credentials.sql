-- Personal connection grants, one row per account and provider, as the
-- credentials.enc state held them. sealed is the credential encrypted by the
-- app under its personal credential key and bound to the row's account and
-- provider; the key stays outside the database. version changes on every
-- write, and a write succeeds only over the version it read.
CREATE TABLE personal_credentials (
    org text NOT NULL,
    owner text COLLATE "C" NOT NULL,
    provider text COLLATE "C" NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    sealed bytea NOT NULL,
    PRIMARY KEY (org, owner, provider)
);
