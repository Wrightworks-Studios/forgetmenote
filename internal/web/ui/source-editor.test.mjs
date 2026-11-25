import test from 'node:test';
import assert from 'node:assert/strict';
import {EditorState} from '@codemirror/state';
import {readFileSync} from 'node:fs';
import {changeSpecs,rangeSelection,takeViewInput,textChanges} from '../../../frontend/source-editor.mjs';

const apply=(text,changes)=>changes.reduce((value,{from,to,insert})=>value.slice(0,from)+insert+value.slice(to),text);
const state=doc=>EditorState.create({doc,extensions:[EditorState.lineSeparator.of('\n')]});

test('CodeMirror edits leave as text changes in engine offsets',()=>{
 let seed=11;const random=n=>{seed=(Math.imul(seed,1664525)+1013904223)>>>0;return seed%n};
 const pieces=['a','😀','\n','\r\n','é',' ','#'];
 for(let i=0;i<500;i++){
  const doc=Array.from({length:random(30)},()=>pieces[random(pieces.length)]).join(''),before=state(doc);
  // Several simultaneous ranges, as one transaction with multiple cursors.
  const specs=[];let at=0;
  while(at<=doc.length && specs.length<4){const from=at+random(5);if(from>doc.length)break;const to=Math.min(doc.length,from+random(4));specs.push({from,to,insert:pieces[random(pieces.length)].repeat(random(3))});at=to+1}
  const tr=before.update({changes:specs});
  assert.equal(apply(doc,textChanges(tr.changes)),tr.state.doc.toString());
  assert.equal(tr.state.doc.length,tr.state.doc.toString().length,'\\r\\n must count as two units');
 }
});
test('text changes from elsewhere apply to CodeMirror in order',()=>{
 const doc='one\r\n\r\ntwo 😀\n',changes=[{from:0,to:3,insert:'ONE'},{from:5,to:5,insert:'inserted '},{from:0,to:0,insert:'# '}];
 const after=state(doc).update(...changeSpecs(changes)).state.doc.toString();
 assert.equal(after,apply(doc,changes));
});
test('setRangeText modes place the selection as a textarea does',()=>{
 assert.deepEqual(rangeSelection('end',4,3).main.toJSON(),{anchor:7,head:7});
 assert.deepEqual(rangeSelection('start',4,3).main.toJSON(),{anchor:4,head:4});
 assert.deepEqual(rangeSelection('select',4,3).main.toJSON(),{anchor:4,head:7});
 assert.equal(rangeSelection('preserve',4,3,undefined),undefined);
});
test('the note editor carries the Markdown language, and no other language pack, so Visual paints formatted at once',async()=>{
 const {build}=await import('esbuild');
 const result=await build({entryPoints:['frontend/source-editor.mjs'],bundle:true,splitting:true,format:'esm',write:false,metafile:true,outdir:'/virtual',logLevel:'silent'});
 const outputs=result.metafile.outputs,[name]=Object.entries(outputs).find(([,output])=>output.entryPoint==='frontend/source-editor.mjs'),inputs=new Set(),seen=new Set();
 (function walk(file){if(seen.has(file))return;seen.add(file);for(const input of Object.keys(outputs[file].inputs))inputs.add(input);for(const item of outputs[file].imports)if(item.kind==='import-statement')walk(item.path)})(name);
 const packs=[...inputs].filter(input=>/node_modules\/@(codemirror\/lang-|lezer\/(python|go|javascript|json|yaml|markdown|html|css))/.test(input));
 assert(packs.includes('node_modules/@codemirror/lang-markdown/dist/index.js'),'the Markdown language loads with the editor');
 // lang-markdown imports the HTML, CSS and JavaScript packs itself; no
 // other language comes with the editor.
 assert.deepEqual(packs.filter(input=>/python|lang-go|lezer\/go|json|yaml/.test(input)),[]);
});
// takeInput (frontend/source-editor.mjs) reads typing the view has not read
// yet through CodeMirror internals; renamed in an upgrade, it would silently
// read nothing, and keystrokes and IME text would be lost again.
test('takeInput reads CodeMirror internals that this version still has',()=>{
 const pinned=JSON.parse(readFileSync('package.json','utf8')).dependencies['@codemirror/view'];
 assert.match(pinned,/^\d+\.\d+\.\d+$/,'@codemirror/view must stay pinned exactly');
 assert.equal(JSON.parse(readFileSync('node_modules/@codemirror/view/package.json','utf8')).version,pinned);
 const source=readFileSync('node_modules/@codemirror/view/dist/index.js','utf8');
 const body=name=>{const start=source.indexOf('\nclass '+name+' ');assert(start>=0,name+' is gone');return source.slice(start,source.indexOf('\nclass ',start+1))};
 const observer=body('DOMObserver'),editor=body('EditorView');
 assert.match(observer,/\n {4}forceFlush\(\) \{/,'DOMObserver.forceFlush() is gone');
 assert.match(observer,/\n {4}pendingRecords\(\) \{[^}]*takeRecords\(\)[^}]*return this\.queue;/,'DOMObserver.pendingRecords() no longer returns the unread records');
 assert.match(editor,/this\.observer = new DOMObserver\(this\)/,'EditorView.observer is gone');
 assert.match(editor,/this\.updateState = 0 \/\* UpdateState\.Idle \*\//,'EditorView.updateState is gone');
});
test('takeInput reads pending input only outside an update, and warns once when it cannot',()=>{
 const view=(records,updateState=0)=>{const self={updateState,flushed:0,observer:{pendingRecords:()=>records,forceFlush:()=>self.flushed++}};return self};
 const pending=view([{}]),idle=view([]),updating=view([{}],2);
 for(const item of [pending,idle,updating])takeViewInput(item);
 assert.deepEqual([pending.flushed,idle.flushed,updating.flushed],[1,0,0]);
 const warn=console.warn,warnings=[];console.warn=(...args)=>warnings.push(args.join(' '));
 try{
  for(const changed of [{updateState:0,observer:{flush(){}}},{updateState:0},{observer:{pendingRecords:()=>[{}],forceFlush(){}}}])takeViewInput(changed);
 }finally{console.warn=warn}
 assert.equal(warnings.length,1);assert.match(warnings[0],/typing may be lost/);
});
