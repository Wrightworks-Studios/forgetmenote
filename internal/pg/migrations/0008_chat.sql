-- Chat conversations, one row per conversation as its state.json was, keyed by
-- the SHA-256 of the owner's stable account ID. record keeps the exact bytes a
-- recovery overlay is bound to, and version changes on every write, so a
-- stream on any gateway polls it instead of the transcript. active marks a
-- conversation with an unfinished run, for admission and reconciliation.
CREATE TABLE chat_conversations (
    org text NOT NULL,
    account text COLLATE "C" NOT NULL,
    id text COLLATE "C" NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    active boolean NOT NULL,
    record bytea NOT NULL,
    recovery bytea,
    PRIMARY KEY (org, account, id)
);
CREATE INDEX chat_conversations_active ON chat_conversations (org, account) WHERE active;
