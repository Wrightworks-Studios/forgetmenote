# Storage v2 cutover

Audience: operators of a single-host instance (the Compose deployment, with its
Postgres service). Storage v2 moves each notebook from its Git repository under
`data/projects/NOTEBOOK/workspace` into the instance database and blob store,
keeping every commit and its SHA ([notebook storage](architecture.md#notebook-storage)).
It happens once per instance, in one window: the instance is stopped for the
import, started paused to check the result, and stopped once more to retire the
previous release's files. The window is short: in a
rehearsal on copies of a production instance's five notebooks (175 commits,
3.4 MB), the import took 1.6 s, verification 0.4 s and the export 3 s. On a synthetic
notebook of 20,000 revisions, 2,000 notes and 200 images, the import took
1 min 24 s to 1 min 34 s, verification 11 to 15 s, and the export 1 min 19 s to
2 min 23 s plus about 40 s to repack and check it, in two runs on a shared host
([rehearsal results](design/storage-v2-results-cutover.md)). On a synthetic
notebook of 5,000 revisions and 500 notes, `check` took 1.9 to 2.2 s, against
0.9 to 1.1 s for `verify`.

An instance whose notebooks live on a notebook owner process (the Duplo Postgres
stack in [moving onto Postgres](../deploy/production/postgres-cutover.md)) moves
with `storage_cutover.py` instead, which runs these commands on the owner's volume
([storage v2 cutover of an owner and gateways](../deploy/production/storage-cutover.md)).

## Before the window

- The instance already runs on Postgres ([Postgres transition](postgres.md)).
- Plan time for the full backups below and keep enough disk for them: the
  database dump, an archive of the whole data directory and one bundle per
  notebook.
- Note the running image so you can return to it. With Compose, tag it before
  you build anything: `docker tag forgetmenote:local forgetmenote:pre-v2`.
- Optionally rehearse: restore a recent full backup (the database dump and the
  data directory) into a separate directory and database, set `OUTGOING=paused`
  and its own `PUBLIC_URL`, and run the import, verification and export below
  against the copy. Then start the new release on the copy, listening on the
  loopback interface only with no published port, and check it as in
  [step 4](#4-start-the-new-release-and-check-it). A rehearsal copies personal content, so
  it needs its own approval. Never run a backup watcher against a copy. The
  [rehearsal on a production copy](design/storage-v2-results-cutover.md) lists
  the checks it ran.

## 1. Freeze

1. Ask people and agents to stop editing.
2. Checkpoint live notes. A note with accepted live edits shows `Live changes
   synced · Save to create a Git version`: Save it, or call
   `checkpoint_live_note` for notes agents edited with `patch_live_note`. The
   import refuses a note whose live edits were not checkpointed and names it,
   so an edit made after this step is caught, not lost.
3. Let each backup worker push the final revision. A watcher's `status.json`
   shows `synced` with the notebook's `HEAD`, and `watcher.py --once --config
   PATH` pushes once and fails if it cannot; a managed backup shows its last
   verified revision in Notebook settings. Managed workers cannot lease while
   outgoing actions are paused, so finish this before the next step. Anything
   committed later is in the bundles below, and the export pushes it after the
   cutover.
4. Pause outgoing actions: set `OUTGOING=paused` in `.env` (or
   `server.outgoing` to `"paused"` in `instance.json`), then
   `docker compose up -d forgetmenote`. The log shows `ForgetMeNote outgoing
   actions are paused`. No automation, Slack message, background review or
   managed backup lease runs from now on, including while the previous release
   runs again to fix a refusal and while the new release is checked
   ([outgoing paused](postgres.md#outgoing-paused)).
5. Stop the backup watchers.

## 2. Final backups

Stop the application and keep Postgres running:

```sh
docker compose stop forgetmenote
```

Then take every backup the rollback needs, and check each one:

```sh
mkdir -p /backup/pre-v2/bundles
# The database, which holds the catalog and the rest of the control plane.
docker compose exec -T postgres pg_dump -U forgetmenote forgetmenote > /backup/pre-v2/forgetmenote.sql
# One complete bundle per notebook repository.
for repo in data/projects/*/workspace; do
  [ -d "$repo/.git" ] || continue
  id=$(basename "$(dirname "$repo")")
  git -C "$repo" -c core.hooksPath=/dev/null bundle create "/backup/pre-v2/bundles/$id.bundle" --all
  git -C "$repo" bundle verify "/backup/pre-v2/bundles/$id.bundle"
done
# The whole data directory, including every .git directory and live state.
tar -czf /backup/pre-v2/data.tar.gz data
tar -tzf /backup/pre-v2/data.tar.gz > /dev/null  # reads the whole archive back
```

Keep `.env`, `secrets/` and any `instance.json` with them. With the GitHub push
from the freeze, every notebook now has an independent copy of its history.
Once [step 5](#5-retire-the-previous-releases-files) retires the previous
release's files, the archive and the bundles are their only copies, which a
[rollback](#rollback) restores: keep them offline.

## 3. Import and verify

Update the source checkout to the storage v2 release and build it:

```sh
docker compose build forgetmenote
```

Run the cutover commands on the stopped instance. The service's entrypoint runs
`forgetmenote migrate` first, which adds the notebook tables (migrations `0017`
and `0018`) and stores live document snapshots uncompressed (`0019`):

```sh
set -o pipefail  # in bash: a pipeline then fails when the command does, not only tee
docker compose run --rm -T forgetmenote admin storage import | tee /backup/pre-v2/import.jsonl
docker compose run --rm -T forgetmenote admin storage verify | tee /backup/pre-v2/verify.jsonl
docker compose run --rm -T forgetmenote admin storage export | tee /backup/pre-v2/export.jsonl
```

Each command prints one JSON line per notebook and fails if any notebook fails;
`-notebook ID` limits it to one. Check each command's exit status, not only its
lines: piped into `tee` without `pipefail`, a failure exits 0. Whatever you ran
with `-notebook`, run `verify` once more without it before [step 4](#4-start-the-new-release-and-check-it):
it checks every notebook again, and fails for one imported earlier whose
repository changed since.

- **import** stores the history of each repository's `HEAD` with every commit
  and tree byte for byte, then verifies it. It imports every repository under
  `data/projects`, including one the catalog does not list. Notes keep their
  `fmn-id` identities; other files, and notes without one, keep theirs through
  every rename Git detects, edits included, so their history reads as before.
  It refuses while any gateway is serving (after a crash, a
  gateway's presence lasts 30 seconds). An import that fails removes what it
  stored, so it can run again. One that stopped without failing, because it
  was killed, ran out of memory, lost the database or the host stopped, left
  part of the notebook's history; running the import again resumes it from
  there (`resumed`), and the new release refuses to serve the notebook until
  then. Otherwise it never imports over a notebook the store already has: one
  at the same head is verified again (`already imported`), any other is
  refused. Once it verified a notebook, it records the commit it imported,
  which `check` compares with the repository's `HEAD`. `notImported` lists
  branches and tags whose commits `HEAD` does not contain; they stay only in
  the data directory and the bundles. It ends by
  gathering the database's statistics on what it stored (`ANALYZE`), which
  autovacuum would gather only later: until then a note's history, the first
  ones the new release lists and those `check` compares, can take a hundred
  times longer.
- **verify** proves that each stored notebook's head is the repository's `HEAD`
  and that its commits, trees and blobs are exactly those `HEAD` reaches.
- **export** writes each notebook's Git export, `data/exports/NOTEBOOK.git`,
  repacks it, checks it with `git fsck --strict`, and verifies it against the
  store, so each export ends at the same commit SHA as the repository it came
  from.

The import refuses a notebook whose previous storage holds work its Git history
lacks, before storing anything of it:

| Refusal | Action |
| --- | --- |
| `an interrupted write is pending (write-journal.json)` | Start the previous release once so it recovers the write, stop it, and import again. |
| `the notebook repository has uncommitted changes` | Inspect `git status` in the repository and commit or remove the changes, with the instance stopped, then import again. |
| `live edits are not checkpointed (PATH)` | Start the previous release (still paused), checkpoint `PATH`, stop it, and import again. |
| `collaboration state FILE: …` or `collaboration append log FILE: …` | The import cannot read that live state, so it cannot tell whether edits are pending. Start the previous release and check that notebook's live notes; keep the file for diagnosis. |
| `the store already has this notebook at …, not at Git HEAD …, and its import finished, so it is never imported over` | The repository changed after the notebook was imported, usually because the previous release ran again and committed to it. Restore the database and import everything again, as below. |
| `the store has this notebook at … from an import that did not finish, but that is not a commit of Git HEAD …'s history` | The notebook changed after its import stopped, so the import cannot resume it. Restore the database and import everything again, as below. |

To run the previous release for one of these, start its tagged image with
`docker tag forgetmenote:pre-v2 forgetmenote:local` and
`docker compose up -d --no-build forgetmenote`; it ignores the new tables. Stop
it and rebuild the storage v2 image (`docker compose build forgetmenote`) before
importing again.
Notebooks already imported are verified again on the next import, but if the
previous release committed to one of them meanwhile, the import refuses it as a
different head, and so do `verify` and the new release's startup. Then restore
the database from `/backup/pre-v2/forgetmenote.sql` (as in [rollback](#rollback))
and import everything again.

## 4. Start the new release and check it

Start the storage v2 release, still paused:

```sh
docker compose up -d forgetmenote
docker compose logs --tail=50 forgetmenote
```

It refuses to start while any notebook in the catalog is still a Git repository
that the store lacks, while a catalog notebook's import did not finish (it
stopped part way, or before it verified the notebook, whether or not the
repository is still there), and while a catalog notebook's repository is at a
commit its stored history lacks: an import that stopped part way, or a commit
the previous release made after the import. The log names that notebook and,
for a repository ahead of the store, the store's head, the repository's `HEAD`
and `forgetmenote admin storage verify`: an import that did not finish resumes
when `forgetmenote admin storage import` runs again; otherwise restore the
database and import everything again ([step 3](#3-import-and-verify)). So a
notebook the import skipped, refused or did not finish is never served empty or
stale. The check only reads each
repository's `HEAD` and never changes it; once a notebook is served it takes new
revisions, and a purge rewrites old ones, and neither stops a later start. A
repository under `data/projects` that the catalog does not list does not stop
it, and is not served.

Then, before anyone writes, check the cutover. `check` is the one cutover
command that runs beside the serving release: it only reads, and changes
nothing.

```sh
set -o pipefail  # in bash: a pipeline then fails when the command does, not only tee
docker compose run --rm -T forgetmenote admin storage check | tee /backup/pre-v2/check.jsonl
```

It checks every notebook the catalog lists and every directory under
`data/projects`, or only the one `-notebook ID` names. It prints one JSON line
per notebook and a last line: `summary` counts each status, `counts` counts
the notebooks `compared` with their previous release repository and those
that `passed`, `failed` and were `skipped`, and `"passed": true` means that it
compared at least one notebook and none failed. Otherwise it exits non-zero:
when anything differs; when a notebook the catalog lists has no previous
release repository and `retire` did not remove it (as with a `-data` that is
not the instance's data directory); when it compared no notebook at all (a run
after [retire](#5-retire-the-previous-releases-files)); and for a `-notebook`
that the catalog, the store and `data/projects` all lack. For each notebook whose
previous release repository is still there it proves:

1. **History.** The store holds exactly the history of the repository's
   `HEAD`: the same head, and every commit, tree and file `HEAD` reaches, each
   object hashing to its ID (as `verify` does). The previous release's files
   hold nothing that history lacks (as the import checks).
2. **Export.** The branch of `data/exports/NOTEBOOK.git` is the repository's
   `HEAD` commit, and `git fsck --strict` passes.
3. **Files.** Every file of `HEAD`'s tree is in the store's files at its head
   with the same content and mode, and reads through the read path the server
   serves notes and attachments with as exactly the bytes Git has. Hidden
   files, executables, symbolic links and files over the byte budget are
   refused by that path as the previous release refused them; they count as
   `refused`, and step 1 proved their bytes.
4. **Note histories.** Each note's whole history, through the walk whose
   newest 50 entries the server lists, names the same commits and paths as the
   repository's, with renames followed as the import follows them: a note's
   `fmn-id`, then the renames Git detects, then identical content, and
   `Move "A" to "B"` commits.

If someone wrote to a notebook after the start, `check` proves the same at the
repository's `HEAD`: the store holds every commit, tree and file `HEAD`
reaches (as `retire` verifies), every file reads at revision `HEAD` as Git has
it, each note's whole history as of `HEAD` matches, and the export's branch is
a revision of the notebook that holds `HEAD`, even if the export has not
caught up with the newest revisions yet. It does so only when `HEAD` is the
commit the import recorded: a repository older than the one the import read,
such as an older copy of the data directory, also holds only commits the
store holds, and fails.

| Status | Meaning |
| --- | --- |
| `passed` | All four hold. The line counts what was compared: the repository's `head`, `commits`, `trees` and `blobs`, the `files` served identical, the files `refused` as before, and the `notes` whose history matched. `notImported` lists refs outside `HEAD`'s history, as the import's line does; `retire` keeps such a notebook. |
| `passed at Git HEAD` | The notebook took revisions after the start, `laterRevisions` of them up to its head, `storeHead`; the repository's `HEAD` is the commit the import recorded, and all four hold there, as above. |
| `failed` | `error` names what differs, below. |
| `skipped: retired` | `retire` removed the notebook's previous release files at the time `retired` gives, so there is nothing left to compare. |
| `skipped: no notebook repository` | A notebook the catalog does not list, without a repository: a directory under `data/projects` without one, or a notebook the store holds that has no directory there, such as a deleted notebook named with `-notebook`. A notebook the catalog lists whose repository is missing fails instead, unless `retire` removed it. |

Every line also says whether the notebook was `compared` and whether the
catalog lists it (`catalog`).

| `error` begins with | Action |
| --- | --- |
| `history: Git HEAD … has N commits the notes store lacks` | The previous release committed after the import, or the import stopped part way. Stop the instance and run the import again, which resumes an import that stopped; otherwise restore the database and import everything again ([step 3](#3-import-and-verify)). |
| `history: the notes store's head … is not Git HEAD …`, `history: the notebook does not match its Git history`, `history: content … is corrupt`, `history: the notes store cannot read the notebook's head` | The store does not hold the repository's history. Stop the instance, restore the database from the dump and import everything again ([step 3](#3-import-and-verify)). |
| `history: Git HEAD … is not …, the commit the import imported` | The store holds the repository's history and more, but the repository is not the one the import read: `-data` is an older copy of the data directory, or the repository's `HEAD` moved back. Run `check` on the data directory the import read. |
| `history: the import recorded no commit it imported` | The notebook took revisions after the start, and an earlier build imported it without recording the commit, so `check` cannot tell those revisions from an older repository. Compare the repository's `HEAD` with the `head` of the notebook's line in `/backup/pre-v2/import.jsonl`: only if they are the same commit were the later revisions written after the start. |
| `its previous release repository, … is missing, and retire never removed it` | Nothing shows what the store should hold. Check that `-data` names the instance's data directory (`check` reads its `projects/`); otherwise the repository was moved or lost, or the notebook was created after the new release started. Restore the repository from the freeze backups to check the notebook. |
| `export: …` | Run `admin storage export`, or `export -recreate` when it refuses the export, with the instance stopped, then check again. |
| `files: …`, `note histories: …` | The release serves a file or a history that differs from the repository's. Keep the files, roll back ([rollback](#rollback)) and report the line. |
| an import refusal, such as `the notebook repository has uncommitted changes` | The previous release's files changed after the import; see the refusals in [step 3](#3-import-and-verify). |
| `no such notebook` | `-notebook` names a notebook that neither the catalog, the store nor `data/projects` holds; check the ID. |

A check that compared no notebook, because every one it found was skipped or
it found none, never passes: its summary line says `"passed": false` and it
fails with `storage check compared no notebook`, followed by how many
notebooks it found and the count of each status. Before `retire`, neither
the catalog nor `data/projects` named a notebook with a repository to compare:
check `-data` and the catalog. After `retire` there is nothing left for
`check` to prove.

Optionally, also check search: the notebook search (MCP `search_notes`) and
the text search of `hybrid_search` work at once. `knowledge_status` shows the
semantic index at the revision the previous release indexed, which stays valid
because revisions keep their SHAs; it indexes what changed after that revision
once outgoing actions resume, which may call the embedding provider. As
before, a search leaves out notes in archive containers unless it includes
archives.

Once `check` passes, make the previous release refuse this instance, so a
container left on the old image, or a rollback of the image alone, can never
serve the notebooks from their old repositories and commit there, where
storage v2 never looks:

```sh
docker compose run --rm forgetmenote migrate -finish-rollout
```

It prints `database requires reader version 2`. From then on the previous
image stops at its entrypoint's `migrate` with `database requires reader version
2; this binary enforces only 1, so it must not serve this instance`. Until this
step the previous release can still run against the database, as the refusals
of [step 3](#3-import-and-verify) need.

## 5. Retire the previous release's files

As soon as `check` passes, retire the previous release's files: each
notebook's `workspace` with its `.git`, `collaboration/` and `captures/`.
Storage v2 does not read them, except each repository's `HEAD`, which its
startup compares with the stored history, and nothing else needs them: the
freeze backups of [step 2](#2-final-backups) hold them for a rollback. While
they stay in the data directory, no [purge](purge.md) reaches them: a purge
after the cutover leaves the purged text in the old working tree and its
`.git` (its plan and receipt say so), and every complete backup of the data
directory (`tar … data`) copies it again.

`retire` refuses while any gateway is serving, so stop the instance for it and
start it again:

```sh
set -o pipefail  # in bash: a pipeline then fails when the command does, not only tee
docker compose stop forgetmenote
docker compose run --rm -T forgetmenote admin storage retire -yes | tee /backup/pre-v2/retire.jsonl
docker compose up -d forgetmenote
```

It stays refused beside a serving gateway because two kinds of gateway use the
files it removes: a storage v2 gateway reads each repository's `HEAD` while it
starts, which a removal under it can fail, and a gateway of the previous
release that is still running, started before
[`-finish-rollout`](#4-start-the-new-release-and-check-it), commits into them;
the database cannot tell which kind is serving. The stop is short: `retire`
verifies again that the store holds each repository's history, in about the
time `verify` takes. It removes nothing without `-yes` (or with `-dry-run`), and
`-notebook ID` limits it to one notebook. For each notebook directory under
`data/projects` it prints one JSON line with the `files` it removes and what it
verified against the store: the repository's `HEAD`, how many of its commits
the store holds (`commits`) and how many a purge rewrote (`rewritten`), and the
`trees` and `blobs` of `HEAD`'s tree. A last line counts each status
(`summary`). The command fails if it kept any notebook.

| Status | Meaning |
| --- | --- |
| `would remove`, `removed` | The notes store holds every commit of the repository's `HEAD` history with the same SHA, or the commit a purge it recorded made of it, and `HEAD`'s tree with every tree and file under it, unless a purge rewrote `HEAD`; every object it stores hashes to its ID. Revisions saved after the cutover do not matter. With `-yes`, the working tree with its `.git`, `collaboration/` and `captures/` are deleted. |
| `kept` | Nothing of the notebook is removed; `error` says why. The store lacks a commit or object of the repository (the previous release committed to it after the import), its import did not finish, or the store does not hold the notebook at all; the repository has refs the import left out (a branch or tag outside `HEAD`'s history, a stash, an annotated tag); or the files hold work the history lacks: an interrupted write (`write-journal.json`), live edits not checkpointed, files `git status --ignored` lists, or a working tree without its `.git`. Inspect the files, keep what you need (`git bundle create FILE --all` keeps every ref), and remove them by hand. |
| `removal failed` | Removing stopped part way, for example on a file the command may not delete. What is left is in `data/projects/NOTEBOOK/.previous-release-retired`, which startup never reads; fix the cause and run `retire -yes` again. |
| `skipped: no previous release files` | The notebook has none, such as one created after the cutover or one already retired. |

`retire -yes` records each removal in the database before it removes anything.
The new release starts without the files: a notebook whose repository is gone
is served from the store, and `check` reports it as `skipped: retired` from
then on. A backup taken after a purge no longer holds the purged content, and
a purge plan no longer warns about the previous release's files. A [rollback](#rollback) still works:
it restores the data directory from `/backup/pre-v2/data.tar.gz`, which holds
every notebook's working tree, `.git`, `collaboration/` and `captures/` as they
were at the freeze, and each bundle in `/backup/pre-v2/bundles` holds its
repository's every ref as a second copy. Keep those backups offline for as
long as you might roll back.

`check` proves the cutover only before this step: keep its output from
[step 4](#4-start-the-new-release-and-check-it) (`/backup/pre-v2/check.jsonl`)
as that proof. Once every repository is retired, `check` has nothing left to
compare and fails with `storage check compared no notebook`. On an instance
retired by an earlier storage v2 release (PRs #70 to #72, including the
owner-and-gateways cutover's retire phase), whose `retire` recorded nothing, `check` cannot tell a retired repository from a missing one:
every notebook the catalog lists fails with `its previous release repository,
…, is missing, and retire never removed it`, a notebook it does not list is
`skipped: no notebook repository`, and the command exits non-zero with
`storage check failed for N of M notebooks`. That is expected there and needs
no action.

## 6. Resume backups and outgoing actions

First install this release's `deploy/git-backup/watcher.py` in place of each
backup watcher's copy (`~/.local/lib/forgetmenote-git-backup/watcher.py` in
its [installation](../deploy/git-backup/README.md#installation)). An older
watcher runs Git as in a working tree, so on a bare export it fails at once
with `fatal: this operation must be run in a work tree`; this one handles both.
Then point each backup watcher at the notebook's export instead of its working
tree (`"repository": "/absolute/path/to/data/exports/NOTEBOOK.git"`), and each
entry of a managed worker's `notebooks` map likewise
([managed backups](managed-backups.md#worker-configuration)). The export
continues the same history, so each push is a fast-forward or already up to
date; an SSH or token watcher can check that now with `--once`. Finally remove
`OUTGOING=paused` and run `docker compose up -d forgetmenote`. Managed workers
lease again from then on; check their next verified push in Notebook settings.

## Rollback

Rollback returns to the previous image and the freeze backups:

1. Stop the application: `docker compose stop forgetmenote`.
2. Restore the database from the freeze dump, which also restores the reader
   version the previous release accepts (`migrate -finish-rollout` raised it
   to 2 in [step 4](#4-start-the-new-release-and-check-it)). Returning to the previous image
   without restoring the database is refused by design: that release would
   serve the notebooks from their old repositories, not what storage v2
   stored.
   ```sh
   docker compose exec -T postgres dropdb -U forgetmenote --force forgetmenote
   docker compose exec -T postgres createdb -U forgetmenote forgetmenote
   docker compose exec -T postgres psql -q -U forgetmenote forgetmenote < /backup/pre-v2/forgetmenote.sql
   ```
3. Move the current `data` directory aside and unpack `/backup/pre-v2/data.tar.gz`
   in its place, preserving ownership. The archive holds the previous release's
   files as they were at the freeze, so this works the same whether or not
   [step 5](#5-retire-the-previous-releases-files) retired them; a repository
   the archive lacks can be cloned from its bundle in `/backup/pre-v2/bundles`.
   Rollback loses anything saved on storage v2; keep the directory you moved
   aside, whose `exports/` hold those commits on top of the previous history,
   for reconciling them by hand.
4. Return the source checkout to the previous release, then
   `docker tag forgetmenote:pre-v2 forgetmenote:local` and
   `docker compose up -d --no-build forgetmenote`.
5. Point the backup watchers back at the working trees, and remove
   `OUTGOING=paused` once the previous release serves as expected.

With an S3 blob store, content blocks the import stored stay under `blocks/` in
the bucket; the previous release never reads them, and they can be deleted.

See [storage versions, recovery and rollback](storage-migrations.md#storage-v2-notebook-content-in-postgres)
for what the new tables hold, and [backups](backups.md) for what a full backup
includes from now on.
