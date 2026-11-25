import test from "node:test";
import assert from "node:assert/strict";
import { lineDiff, citationTarget, indexMessage, historyResultDetail } from "./knowledge.mjs";
test("proposal diffs preserve context and distinguish insertions and removals", () => {
  assert.deepEqual(lineDiff("A\nold\nC", "A\nnew\nC"), [{ kind: "context", text: "A" }, { kind: "removed", text: "old" }, { kind: "added", text: "new" }, { kind: "context", text: "C" }]);
});
test("citations retain pinned revisions and reject unsafe destinations", () => {
  assert.deepEqual(citationTarget("notes/a.md", "../source.md?fmn-rev=" + "a".repeat(40) + "#proof"), { path: "source.md", hash: "proof", revision: "a".repeat(40) });
  assert.equal(citationTarget("a.md", "javascript:alert(1)"), null);
});
test("agent document vectors do not claim browser search is ready", () => {
  assert.match(indexMessage({ state: "ready", indexed: 5, total: 5, browserSearch: false }), /query.*endpoint/i);
});

test("unconfigured search reports disabled without claiming an index exists", () => {
  const message = indexMessage({state: "disabled", config: {model: ""}});
  assert.match(message, /not enabled/i);
  assert.doesNotMatch(message, /ready for semantic search/i);
});
test("historical labels report total provenance when the response is bounded",()=>{
 assert.match(historyResultDetail({path:'a.md',revision:'a'.repeat(40),committedAt:'2026-01-01T00:00:00Z',occurrenceCount:900,occurrences:Array(256),occurrencesTruncated:true}),/900 recorded versions/);
});
