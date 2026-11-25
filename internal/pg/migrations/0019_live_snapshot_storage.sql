-- A live document's snapshot is rewritten whole at every compaction and
-- checkpoint, so it is stored out of line without compression: compressing
-- the snapshot of a 1.2 MB manuscript cost about 20 ms of each checkpoint,
-- against the 30 ms checkpoint budget (docs/architecture.md).
-- Snapshots written before this keep their compression until rewritten.
ALTER TABLE live_docs ALTER COLUMN snapshot SET STORAGE EXTERNAL;
