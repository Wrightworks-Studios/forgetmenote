-- Leases name what one gateway does at a time: a background duty, or a
-- gateway's own presence as gateway/<holder>. A holder renews its lease before
-- expires; another takes a duty over only once it has expired, which raises
-- the generation, so a write that checks the generation in its transaction
-- fences a holder that lost it. Work a gateway claims records its holder, and
-- others recover that work only while the holder's presence is gone.
CREATE TABLE leases (
    org text NOT NULL,
    name text COLLATE "C" NOT NULL,
    holder text COLLATE "C" NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    expires timestamptz NOT NULL,
    PRIMARY KEY (org, name)
);
