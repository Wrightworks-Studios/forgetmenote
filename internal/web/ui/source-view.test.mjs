import test from 'node:test';
import assert from 'node:assert/strict';
import {build} from 'esbuild';
import {EditorState} from '@codemirror/state';
import {ensureSyntaxTree} from '@codemirror/language';
import {languageFor} from '../../../frontend/source-view.mjs';

// The note editor (source-editor.mjs) carries the Markdown language on
// purpose, so Visual's first paint is formatted (source-editor.test.mjs).
test('language packs load per language and stay out of the viewer’s chunk',async()=>{
 assert.equal(languageFor('notes.txt'),null);
 for(const path of ['a.py','a.js','a.tsx','a.go','a.json','a.yaml','a.md'])assert.ok((await languageFor(path)()).language,path);
 const result=await build({entryPoints:['frontend/source-view.mjs'],bundle:true,splitting:true,format:'esm',write:false,metafile:true,outdir:'/virtual',logLevel:'silent'});
 const outputs=result.metafile.outputs;
 for(const entry of ['frontend/source-view.mjs']){
  const [name]=Object.entries(outputs).find(([,output])=>output.entryPoint===entry),inputs=new Set(),seen=new Set();
  (function walk(file){if(seen.has(file))return;seen.add(file);for(const input of Object.keys(outputs[file].inputs))inputs.add(input);for(const item of outputs[file].imports)if(item.kind==='import-statement')walk(item.path)})(name);
  const packs=[...inputs].filter(input=>/node_modules\/@(codemirror\/lang-|lezer\/(python|go|javascript|json|yaml|markdown|html|css))/.test(input));
  assert.deepEqual(packs,[],entry+' loads language packs eagerly');
  assert(outputs[name].imports.some(item=>item.kind==='dynamic-import'),entry);
 }
});

// Lezer's HardBreak parser costs the square of a run of spaces that does not
// end its line; the viewer's Markdown reads such a run at once.
test('a Markdown file with a long run of spaces in a line highlights at once',async()=>{
 const language=await languageFor('a.md')(),doc='A line with'+' '.repeat(200000)+'padding.';
 const started=performance.now(),state=EditorState.create({doc,extensions:[language]}),tree=ensureSyntaxTree(state,doc.length,5000),ms=performance.now()-started;
 assert.equal(tree?.length,doc.length);
 assert.ok(ms<500,ms.toFixed(1)+' ms');
});
