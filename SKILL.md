---
name: forgetmenote-development
description: Implement and review ForgetMeNote behavior with test-driven development, simple Go code, and portable Git-backed storage. Use for changes to this application's backend, editor, MCP tools, or deployment.
---

# Develop ForgetMeNote

Read AGENTS.md and the affected caller, implementation, tests, and configuration.

1. State the observable behavior being changed and where it belongs. Prefer an existing owner.
2. Write a small test through the public boundary. For storage, use real Git in a temporary workspace; for HTTP, use httptest.
3. Run it and inspect the failure. A broken fixture or compiler failure is not evidence that the desired behavior fails.
4. Implement the direct solution. Extract a helper only when it names meaningful behavior or removes present duplication.
5. Run the focused test, then refactor with the test green. Keep errors actionable and propagate them to the caller.
6. Run relevant regression checks and inspect the diff. Record checks, remaining risks, and unverified deployment behavior.

For edits, verify both file contents and Git history. For concurrent edits to the same note, prove that exactly one update wins and the losing draft remains recoverable. Edits to different notes must both succeed. For failed saves, prove the original file and repository remain intact. For backups, restore into an independent directory and read the result.

Keep authentication outside content handling. Never let a tool argument select arbitrary filesystem roots, shell commands, remotes, or credentials. Browser and MCP callers must reach the same permission and revision checks.

Prefer local HTML templates and small browser modules. Verify important editor behavior in a real browser in addition to unit tests. Docker checks must use synthetic workspaces, never the author's notes.
