-- Automation state by name, as the files of the automations directory held it:
-- runs, per-job checkpoints and cursors, effect reservations and the configured
-- job definitions. Bodies keep their exact bytes; the version changes on every
-- write, so a scan rereads only changed records.
CREATE TABLE automation_records (
    org text NOT NULL,
    name text COLLATE "C" NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    body bytea NOT NULL,
    PRIMARY KEY (org, name)
);
