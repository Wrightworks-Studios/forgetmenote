import { test } from "node:test";
import assert from "node:assert/strict";
import { resolveLink, restoreDraft } from "./editor.mjs";
test("relative note and attachment links stay in the workspace", () => {
  assert.equal(
    resolveLink("notes/one.md", "../assets/map.png").path,
    "assets/map.png",
  );
  assert.equal(
    resolveLink("notes/one.md", "two.md#heading").path,
    "notes/two.md",
  );
  assert.equal(resolveLink("notes/one.md", "../../outside.md"), null);
  assert.equal(resolveLink("notes/one.md", "javascript:alert(1)"), null);
  assert.equal(
    resolveLink("notes/one.md", "https://example.com").external,
    true,
  );
});
test("drafts preserve their original revision instead of claiming the latest", () => {
  const saved = { content: "server", revision: "new" };
  const draft = { content: "my work", revision: "old" };
  assert.deepEqual(restoreDraft(saved, JSON.stringify(draft)), {
    content: "my work",
    revision: "old",
    conflict: true,
  });
  assert.equal(restoreDraft(saved, "broken").content, "server");
});

test("draft recovery ignores unrelated commits but detects a changed note", () => {
  const draft = {content:"draft",revision:"snapshot-a",version:"note-v1"};
  const same = restoreDraft({content:"saved",revision:"snapshot-b",version:"note-v1"},JSON.stringify(draft));
  assert.equal(same.conflict,false);
  assert.equal(same.revision,"snapshot-a");
  assert.equal(restoreDraft({content:"changed",revision:"snapshot-c",version:"note-v2"},JSON.stringify(draft)).conflict,true);
});

import {movedPath, noteTitle} from './editor.mjs';
test('moves remap only the affected descendants', () => {
  assert.equal(movedPath('notes/a.md','notes','research'), 'research/a.md');
  assert.equal(movedPath('notes-other/a.md','notes','research'), 'notes-other/a.md');
});

import {fileActionTarget, destinationFolders, copyName} from './editor.mjs';
test('file actions constrain names and destinations', () => {
  const node={path:'notes/a.md',kind:'note'};
  assert.equal(fileActionTarget(node,'rename','elsewhere','New name').path,'notes/New name.md');
  assert.equal(fileActionTarget(node,'move','','ignored').path,'a.md');
  assert.equal(fileActionTarget(node,'duplicate','copies','Copy.md').path,'copies/Copy.md');
  for(const name of ['../bad','sub/note','.hidden','', 'bad\\name']) assert.ok(fileActionTarget(node,'rename','',name).error);
  assert.ok(fileActionTarget({path:'notes',kind:'folder'},'move','notes/child','').error);
  assert.ok(fileActionTarget(node,'move','notes','').error);
});
test('folder picker is derived from notes and copy names avoid collisions', () => {
  const entries=[{path:'notes/a.md'},{path:'notes/a copy.md'},{path:'wiki/people/a.md'},{path:'comments/hidden.json'}];
  assert.deepEqual(destinationFolders(entries),['','notes','wiki','wiki/people']);
  assert.equal(copyName({path:'notes/a.md',kind:'note'},entries),'a copy 2');
});

import {blockingFileDrafts} from './editor.mjs';
test('opening file actions leaves drafts alone and never requires reconciliation', () => {
  const drafts = ['reference-templates/research-note.md'];
  assert.deepEqual(blockingFileDrafts(undefined, 'editorial/index.md', '', drafts), []);
  assert.deepEqual(drafts, ['reference-templates/research-note.md']);
});
test('duplication guards source and destination drafts without blocking unrelated notes', () => {
  const drafts = ['notes/one.md', 'notes/nested/two.md', 'notes-other/three.md', 'copy/unsaved.md'];
  assert.deepEqual(blockingFileDrafts('duplicate', 'notes', 'copy', drafts), ['notes/one.md', 'notes/nested/two.md', 'copy/unsaved.md']);
  assert.deepEqual(blockingFileDrafts('duplicate', 'notes/one.md', 'new.md', drafts), ['notes/one.md']);
  assert.deepEqual(blockingFileDrafts('duplicate', 'editorial/index.md', 'editorial/copy.md', drafts), []);
});
test('moves and renames guard all project drafts because links elsewhere may change', () => {
  const drafts = ['reference-templates/research-note.md'];
  for (const action of ['move', 'rename']) assert.deepEqual(blockingFileDrafts(action, 'editorial/index.md', 'elsewhere.md', drafts), drafts);
});

import {contextMenuPosition} from './editor.mjs';
test('context menus stay by their trigger and within small viewport edges', () => {
  assert.deepEqual(contextMenuPosition({right:260,top:100,bottom:126}, {width:170,height:120}, {width:800,height:600}), {left:90,top:130});
  assert.deepEqual(contextMenuPosition({right:40,top:540,bottom:566}, {width:170,height:120}, {width:320,height:600}), {left:8,top:416});
  assert.deepEqual(contextMenuPosition({right:850,top:2,bottom:28}, {width:170,height:120}, {width:800,height:100}), {left:622,top:8});
});

test('display titles prefer explicit metadata, saved headings, then filenames', () => {
 assert.equal(noteTitle({path:'opaque.md',title:'Recognizable heading'}),'Recognizable heading');
 assert.equal(noteTitle({path:'explicit.md',title:'Heading',metadata:{title:'Chosen title'}}),'Chosen title');
 assert.equal(noteTitle({path:'folder/index.md'}),'Overview');
 assert.equal(noteTitle({path:'folder/Field notes.md'}),'Field notes');
});

test('notebook links preserve identity and cannot disguise an external origin',async()=>{
 const {notebookLink}=await import('./editor.mjs');
 const id='1234567890abcdef1234567890abcdef';
 assert.deepEqual(notebookLink('/?project=second&note=same.md&noteId='+id+'#heading','https://notes.example'),{project:'second',path:'same.md',noteId:id,revision:'',hash:'heading'});
 assert.equal(notebookLink('https://evil.example/?project=second&note=same.md','https://notes.example'),null);
 assert.equal(notebookLink('/?project=../../private&note=same.md','https://notes.example'),null);
});
