-- Memory became on by default in November 2026. Assistant settings stored
-- before then hold the old default, off, whether or not the person chose it,
-- so this turns every stored setting on once. A setting saved later keeps its
-- value, and older binaries keep the stored value too. The catalog package
-- owns this document; the version bump makes every gateway reload it, and a
-- write computed from the old version conflicts and is retried.
UPDATE documents
SET body = jsonb_set(body, '{data,assistants}', (
        SELECT jsonb_object_agg(key, value || '{"maintainMemory": true}'::jsonb)
        FROM jsonb_each(body->'data'->'assistants'))),
    version = version + 1,
    updated_at = now()
WHERE kind = 'catalog' AND id = 'state'
    AND jsonb_typeof(body->'data'->'assistants') = 'object'
    AND EXISTS (
        SELECT 1 FROM jsonb_each(body->'data'->'assistants')
        WHERE value->'maintainMemory' IS DISTINCT FROM 'true'::jsonb);
