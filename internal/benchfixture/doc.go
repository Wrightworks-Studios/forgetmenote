// Package benchfixture generates the storage benchmark fixtures, on which the
// performance budgets in docs/architecture.md are measured:
//
//   - F1, Novel: Moby-Dick as one 1.2 MB manuscript note, pinned by SHA-256;
//   - F1h, NovelHistory: thousands of edits to F1 by three humans and two agents;
//   - F2, Notebook: 2,000 notes, 200 images, comments, locks and Trash over
//     20,000 commits and 18 months;
//   - T1, Traces: browser typing, paste, chapter delete, IME and concurrent
//     edit traces against F1.
//
// F1h and F2 are streams of Revisions that any store can replay; WriteGit
// and WriteFiles materialize them, and cmd/fixture does so from the shell.
//
// The F1 source is never committed. Novel reads it from FMN_FIXTURE_CACHE
// (default: forgetmenote-fixtures in the user cache directory) and downloads
// it there only when FMN_FIXTURE_FETCH=1. Tests that need it skip without it.
// Containers that mount only the worktree can use the ignored .fixture-cache
// directory at the repository root as the cache.
//
// Tests and benchmarks use this package; production code never
// imports it (TestNoProductionImports enforces this). It depends only on the
// standard library so that any package's tests can use it without import
// cycles; the notebook formats it writes are mirrored from internal/workspace
// and checked against that package by this package's tests.
//
// Every generator is a pure function of its options: the same seed yields the
// same bytes, commit IDs and traces on every platform and Go release. The
// generators therefore use their own PRNG and integer arithmetic only.
package benchfixture
