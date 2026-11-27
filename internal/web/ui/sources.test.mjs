import test from 'node:test';
import assert from 'node:assert/strict';
import {sourceCitation, originalURL} from './sources.mjs';
const revision='a'.repeat(40);
test('source citations pin the record and selected PDF page',()=>{
 assert.equal(sourceCitation({path:'sources/a b.md',revision,metadata:{title:'Reference [A]'}},'page-3'),`[Reference \\[A\\]](/sources/a%20b.md?fmn-rev=${revision}#page-3)`);
 assert.throws(()=>sourceCitation({path:'x.md',revision:''}));
});
test('original source downloads stay scoped and pinned; unsafe provenance is inert',()=>{
 assert.equal(originalURL('private',{original:'assets/sources/a/original.html'},revision),`/p/private/assets/assets/sources/a/original.html?revision=${revision}`);
 for(const original of ['https://evil.test/a.html','assets/sources/../secret.pdf','assets/sources/%2e%2e/a.pdf','assets/sources/a.svg','assets/sources/a.html?x']) assert.equal(originalURL('private',{original},revision),null);
});

test('document and spreadsheet originals retain pinned download links',()=>{
 for(const ext of ['txt','docx','epub','xlsx','csv','tsv','png','jpg','jpeg','tiff','tif','webp','bmp','pptx','eml','msg','odt','ods','odp','pages','key','wav','mp3','m4a','aac','ogg','flac','mp4','mov','avi']) assert.equal(originalURL('private',{original:`assets/sources/a/original.${ext}`},revision),`/p/private/assets/assets/sources/a/original.${ext}?revision=${revision}`);
});

test('upload and URL capture keep their inputs separate and submit only the active URL', async()=>{
 const {initSources}=await import('./sources.mjs');
 const elements=new Map();
 const element=id=>{if(!elements.has(id))elements.set(id,{value:'',hidden:false,disabled:false,checked:false,required:false,textContent:'',files:[],replaceChildren(...children){this.children=children;},addEventListener(){},dataset:{},classList:{toggle(){}},setAttribute(name,value){this[name]=value;},showModal(){this.open=true;},close(){this.open=false;}});return elements.get(id);};
 const previousOption=globalThis.Option;globalThis.Option=class {constructor(text,value){this.text=text;this.value=value;}};
 const previous=globalThis.document;globalThis.document={getElementById:element};
 const previousReader=globalThis.FileReader;globalThis.FileReader=class{readAsDataURL(){this.result='data:text/plain;base64,QW1iZXI=';this.onload();}};
 const sent=[];
 try {
 const sources=initSources({api:async(path)=>path.endsWith('/notes') ? [{path:'research/reference.md'},{path:'notes/start.md'}] : [{extensions:['.png'],mime:'image/png',extractedBy:'agent',maxBytes:20*1024*1024},{extensions:['.txt'],extractedBy:'server',maxBytes:20*1024*1024}],post:async(path,input)=>{sent.push(input);throw new Error('preview stopped by fixture');},getProject:()=>({id:'test'}),getNote:()=>null,getFolder:()=> 'research',canEdit:()=>true,dirty:()=>false});
 await sources.open();
 assert.equal(element('archive-folder').value,'research');
 assert.deepEqual(element('archive-folder').children.map(option=>option.value),['','notes','research']);
 assert.equal(element('archive-folder-field').hidden,false);
 assert.equal(element('archive-url-field').hidden,true,'upload should not show the capture URL');
 assert.equal(element('archive-origin-details').open,false,'upload attribution should start collapsed');
 element('archive-origin-url').value='https://example.org/upload-origin';
 element('archive-fetch').checked=true;element('archive-upload').checked=false;
 element('archive-fetch').onchange();
 assert.equal(element('archive-file-field').hidden,true);
 assert.equal(element('archive-url-field').hidden,false);
 assert.equal(element('archive-url').required,true);
 assert.equal(element('archive-origin-url').disabled,true);
 element('archive-url').value='https://example.org/capture';
 await element('archive-form').onsubmit({preventDefault(){}});
 assert.deepEqual(sent,[{folder:'research',url:'https://example.org/capture'}]);
 // An image is stored for a connected agent to read; nothing asks to upload it elsewhere.
 element('archive-url').value='https://example.org/image.png';element('archive-url').oninput();
 await element('archive-form').onsubmit({preventDefault(){}});
 assert.deepEqual(sent[1],{folder:'research',url:'https://example.org/image.png'});
 element('archive-url').value='https://example.org/note.txt';element('archive-url').oninput();
 await element('archive-form').onsubmit({preventDefault(){}});
 assert.deepEqual(sent[2],{folder:'research',url:'https://example.org/note.txt'});

 element('archive-fetch').checked=false;element('archive-upload').checked=true;
 element('archive-upload').onchange();
 assert.equal(element('archive-url').disabled,true);
 assert.equal(element('archive-file').required,true);
 assert.equal(element('archive-origin-url').value,'https://example.org/upload-origin');
 assert.equal(element('archive-formats').textContent,'Documents, images, email, audio, and video · Up to 20 MiB');
 element('archive-file').files=[{name:'large.txt',size:18*1024*1024}];
 await element('archive-form').onsubmit({preventDefault(){}});
 assert.equal(sent.length,4,'configured budget accepts a file beyond the old 16 MiB limit');
 element('archive-file').files=[{name:'too-large.txt',size:21*1024*1024}];
 await element('archive-form').onsubmit({preventDefault(){}});
 assert.equal(sent.length,4,'over-budget file must be refused before reading it');
 assert.match(element('archive-error').textContent,/20 MiB/);
 await sources.open({note:{path:'sources/a.md',revision:'a'.repeat(40),metadata:{source:{method:'uploaded',url:'https://example.org/recorded'}}}});
 assert.equal(element('archive-folder-field').hidden,true,'refresh keeps the current location');
 assert.equal(element('archive-origin-url').value,'https://example.org/recorded');
 assert.equal(element('archive-origin-details').open,true,'recorded provenance remains visible when replacing a file');
 } finally {globalThis.document=previous;globalThis.Option=previousOption;globalThis.FileReader=previousReader;}
});

test('source audit details stay collapsed while warnings and verification problems remain visible', async()=>{
 const {initSources}=await import('./sources.mjs');
 const make=tag=>({tag,children:[],textContent:'',hidden:false,open:false,append(...nodes){for(const n of nodes){n.parent=this;this.children.push(n);}},replaceChildren(...nodes){this.children=[];this.append(...nodes);},add(n){this.append(n);},setAttribute(name,value){this[name]=value;},addEventListener(){}});
 const all=n=>[n,...n.children.flatMap(all)];
 const hasAncestor=(node,ancestor)=>node===ancestor || !!node.parent && hasAncestor(node.parent,ancestor);
 const old={document:globalThis.document,Option:globalThis.Option,DOMParser:globalThis.DOMParser};
 const ids=new Map();const el=id=>{if(!ids.has(id))ids.set(id,make(id));return ids.get(id);};
 globalThis.document={getElementById:el,createElement:make};globalThis.Option=class {constructor(){Object.assign(this,make('option'));}};
 globalThis.DOMParser=class{parseFromString(){return {querySelectorAll:()=>[]};}};
 let response={originalState:'Original checksum matches', originalStatus: 'match',textState:'Extracted text checksum matches', textStatus: 'match'};
 const record={original:'assets/sources/a/original.pdf',url:'https://example.org/reference',capturedAt:'2026-09-08T12:00:00Z',warning:'Scanned pages were not extracted.',extractor:'PDF extractor',sha256:'original-hash',textSHA256:'text-hash',extraction:'partial'};
 try {
 const sources=initSources({api:async()=>{if(response instanceof Error)throw response;return response;},getProject:()=>({id:'test'})});
 const note={path:'references/a.md',revision,metadata:{source:record}};
 await sources.render(note);
 const panel=el('archive-details');let nodes=all(panel);
 const technical=nodes.find(n=>n.tag==='details');
 assert.ok(technical,'technical details disclosure exists');assert.equal(technical.open,false);
 for(const text of ['original-hash','text-hash','PDF extractor','Original checksum matches · Extracted text checksum matches']){
  const item=nodes.find(n=>n.textContent===text);assert.ok(item,text);assert.ok(hasAncestor(item,technical),'audit data should be inside the disclosure');
 }
 const warning=nodes.find(n=>n.textContent===record.warning);assert.ok(warning);assert.equal(hasAncestor(warning,technical),false);
 response={originalState:'Original checksum mismatch', originalStatus: 'changed',textState:'Extracted text changed', textStatus: 'changed'};
 await sources.render(note);nodes=all(panel);
 assert.match(panel.children[0].textContent,/Content changed/,'collapsed source summary signals changes');
 assert.ok(nodes.some(n=>n.role==='status' && !n.hidden && /changed|differ/i.test(n.textContent)));
 response=new Error('offline');await sources.render(note);nodes=all(panel);
 assert.match(panel.children[0].textContent,/Verification unavailable/);
 assert.ok(nodes.some(n=>n.role==='status' && !n.hidden && /Could not verify/.test(n.textContent)));
 response={originalState:'Original checksum matches', originalStatus: 'match',textState:'Extracted text checksum matches', textStatus: 'match'};
 await sources.render({...note,editable:true});nodes=all(panel);
 assert.ok(nodes.some(n=>n.textContent==='Update source'));
 // Without the sources module the note keeps its provenance and original,
 // and is neither updated nor verified.
 let verified=0;const off=initSources({api:async()=>{verified++;return response;},getProject:()=>({id:'test'}),available:()=>false});
 await off.render({...note,editable:true});nodes=all(panel);
 assert.equal(verified,0);
 assert.ok(nodes.some(n=>n.textContent==='The sources module is turned off on this instance, so this capture is not verified.'));
 assert.equal(nodes.some(n=>n.textContent==='Update source'),false);
 assert.ok(nodes.some(n=>n.textContent==='Download original'));
 }finally{Object.assign(globalThis,old);}
});

test('durable source preview saves by job ID without resending or re-extracting its upload', async()=>{
 const {initSources}=await import('./sources.mjs');
 const elements=new Map();
 const element=id=>{if(!elements.has(id))elements.set(id,{value:'',hidden:false,disabled:false,checked:false,files:[],replaceChildren(){},querySelectorAll(){return [];},addEventListener(){},dataset:{},classList:{toggle(){}},setAttribute(name,value){this[name]=value;},showModal(){this.open=true;}});return elements.get(id);};
 const previous={document:globalThis.document,Option:globalThis.Option,FileReader:globalThis.FileReader,localStorage:globalThis.localStorage};
 globalThis.document={getElementById:element};globalThis.Option=class{};
 globalThis.FileReader=class {readAsDataURL(){this.result='data:text/plain;base64,QW1iZXI=';this.onload();}};
 const remembered=new Map();globalThis.localStorage={getItem:key=>remembered.get(key)||null,setItem:(key,value)=>remembered.set(key,value),removeItem:key=>remembered.delete(key)};
 const sent=[];const id='a'.repeat(32);
 try {
  const sources=initSources({api:async path=>path.endsWith('/notes')?[]:path.endsWith('/formats')?[{extensions:['.txt'],maxBytes:64*1024*1024}]:{job:{id,state:'ready',pages:0},prepared:{capture:{title:'Evidence',record:{warning:'Original retained'}},html:'<p>Amber evidence</p>'}},post:async(path,input)=>{sent.push({path,input});return path.endsWith('/jobs')?{job:{id,state:'queued',pages:0}}:{path:'sources/evidence.md',revision,metadata:{title:'Evidence'}};},getProject:()=>({id:'test'}),getNote:()=>null,canEdit:()=>true,dirty:()=>false,refresh:async()=>{}});
  await sources.open();element('archive-file').files=[{name:'evidence.txt',size:5}];
  assert.equal(element('archive-formats').textContent,'Documents, images, email, audio, and video · Up to 64 MiB','every build stores images, audio and video for an agent to read');
  await element('archive-form').onsubmit({preventDefault(){}});
  assert.equal(element('archive-save').hidden,false);assert.match(element('archive-job-status').textContent,/Ready/);
  await element('archive-save').onclick();
  assert.equal(sent.length,2);assert.equal(sent[0].input.data,'QW1iZXI=');assert.equal(sent[1].input.job,id);
  assert.equal(sent[1].input.data,undefined);assert.equal(sent[1].input.prepared,undefined);assert.equal(remembered.size,0);
 } finally {Object.assign(globalThis,previous);}
});

test('an upload stored without text is read by the chat, or the dialog says why it waits', async()=>{
 const {initSources,awaitsChat}=await import('./sources.mjs');
 const elements=new Map();
 const element=id=>{if(!elements.has(id))elements.set(id,{value:'',hidden:false,disabled:false,checked:false,files:[],textContent:'',replaceChildren(){},querySelectorAll(){return [];},addEventListener(){},dataset:{},classList:{toggle(){}},setAttribute(name,value){this[name]=value;},showModal(){this.open=true;}});return elements.get(id);};
 const previous={document:globalThis.document,Option:globalThis.Option,FileReader:globalThis.FileReader,localStorage:globalThis.localStorage};
 globalThis.document={getElementById:element};globalThis.Option=class{};
 globalThis.FileReader=class {readAsDataURL(){this.result='data:image/png;base64,iVBORw==';this.onload();}};
 globalThis.localStorage={getItem:()=>null,setItem(){},removeItem(){}};
 const id='a'.repeat(32),read='b'.repeat(40);
 const stored={path:'sources/notice.md',revision,metadata:{title:'Notice',source:{method:'uploaded',extraction:'original-only'}}};
 let sent=[],chat={note:{path:'sources/notice.md',revision:read,metadata:{title:'Notice',source:{method:'uploaded',extraction:'client-supplied'}}}},refreshed=0;
 try {
  const sources=initSources({api:async path=>path.endsWith('/notes')?[]:path.endsWith('/formats')?[{extensions:['.png'],maxBytes:64*1024*1024}]:{job:{id,state:'ready',pages:0},prepared:{capture:{title:'Notice',record:{warning:'Stored as the original'}},html:''}},post:async(path,input)=>{sent.push({path,input});return path.endsWith('/jobs')?{job:{id,state:'queued',pages:0}}:path.endsWith('/import')?stored:chat;},getProject:()=>({id:'test'}),getNote:()=>null,canEdit:()=>true,dirty:()=>false,refresh:async()=>{refreshed++;}});
  const archive=async()=>{sent=[];await sources.open();element('archive-file').files=[{name:'notice.png',size:5}];await element('archive-form').onsubmit({preventDefault(){}});await element('archive-save').onclick();};
  await archive();
  assert.deepEqual(sent.map(call=>call.path),['/p/test/api/sources/jobs','/p/test/api/sources/import','/p/test/api/sources/chat-text']);
  assert.deepEqual(sent[2].input,{path:'sources/notice.md',revision},'the chat reads the stored revision');
  assert.match(element('archive-result').textContent,new RegExp(`fmn-rev=${read}`),'the citation pins the revision with the text');
  assert.match(element('archive-chat-status').textContent,/^The chat read this upload/);
  assert.equal(element('archive-chat-status').hidden,false);
  assert.equal(element('archive-copy').disabled,false);assert.equal(refreshed,2);
  // When the chat does not read it, the uploader is told why.
  chat={waiting:'The chat reads PNG and JPEG images only. A connected agent can add its text (list_sources_awaiting_text).'};
  await archive();
  assert.equal(element('archive-chat-status').textContent,'Stored without text. '+chat.waiting);
  assert.match(element('archive-result').textContent,new RegExp(`fmn-rev=${revision}`));
  chat=Promise.reject(new Error('Forbidden'));chat.catch(()=>{});
  await archive();
  assert.equal(element('archive-chat-status').textContent,'Stored without text. The chat did not read it: Forbidden');
  // A source the server read, or one it fetched, goes to no chat.
  assert.equal(awaitsChat({metadata:{source:{method:'uploaded',extraction:'extracted'}}}),false);
  assert.equal(awaitsChat({metadata:{source:{method:'fetched',extraction:'original-only'}}}),false);
  assert.equal(awaitsChat({metadata:{source:{method:'uploaded',extraction:'partial'}}}),true);
 } finally {Object.assign(globalThis,previous);}
});
