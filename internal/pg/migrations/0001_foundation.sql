-- The oldest binary meaning this database still accepts; see docs/postgres.md.
CREATE TABLE instance_reader (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    min_version integer NOT NULL CHECK (min_version > 0)
);
INSERT INTO instance_reader (min_version) VALUES (1);
