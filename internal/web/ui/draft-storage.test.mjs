import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {DraftStorage} from './draft-storage.mjs';
const memory=()=>{const data={};Object.defineProperties(data,{getItem:{configurable:true,value:key=>data[key]??null},setItem:{configurable:true,value:(key,value)=>{data[key]=String(value)}},removeItem:{value:key=>{delete data[key]}}});return data};
const draft=content=>({content,revision:'git',version:'blob',private:true}),raw=content=>JSON.stringify(draft(content));
const owner=letter=>'00000000-0000-4000-8000-00000000000'+(letter==='a'?'1':'2');
test('tab cleanup cannot erase another independent draft live replay or scratch',()=>{
 const storage=memory(),a=new DraftStorage(storage,owner('a')),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 for(const kind of ['draft','live','reconcile']){const value=kind==='reconcile'?raw:draft;a.write(kind,scope,value('A'));b.write(kind,scope,value('B'));b.remove(kind,scope);assert.equal(JSON.parse(a.read(kind,scope)).content,'A')}
 assert.deepEqual(a.paths('u/p/'),['n.md']);assert.equal(b.candidates(scope)[0].draft.content,'A');
});
test('adopting a foreign candidate archives own differing work and explicit resolution preserves its owner',()=>{
 const storage=memory(),session=memory(),a=new DraftStorage(storage,owner('a'),session),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 a.write('draft',scope,draft('A'));b.write('draft',scope,draft('B'));b.write('live',scope,'replay B');
 const candidate=b.candidates(scope)[0];a.adopt(scope,candidate);a.write('draft',scope,draft('C'));
 assert.deepEqual(new Set(a.candidates(scope).map(c=>c.draft.content)),new Set(['A','B','C']));
 a.remove('draft',scope);a.resolve(scope);assert.equal(JSON.parse(b.read('draft',scope)).content,'B');assert.equal(b.read('live',scope),JSON.stringify('replay B'));
 const reloaded=new DraftStorage(storage,owner('a'),session);assert.deepEqual(reloaded.candidates(scope).map(c=>c.draft.content),['A']);
 b.write('draft',scope,draft('newer B'));assert(reloaded.candidates(scope).some(c=>c.draft.content==='newer B'));
});
test('legacy records remain recoverable and block organizing until explicit resolution',()=>{
 const storage=memory(),a=new DraftStorage(storage,owner('a')),scope='@local-admin/p/n.md';storage.setItem('gitnotes:draft:'+scope,raw('legacy'));storage.setItem('gitnotes:live:'+scope,'legacy replay');
 a.remove('draft',scope);assert.equal(a.candidates(scope)[0].draft.content,'legacy');a.adopt(scope,a.candidates(scope)[0]);assert.equal(a.read('live',scope),null);assert.equal(a.live(scope),'legacy replay');
 assert.deepEqual(a.paths('@local-admin/p/'),['n.md']);a.remove('draft',scope);a.resolve(scope);assert.deepEqual(a.paths('@local-admin/p/'),[]);assert.equal(storage.getItem('gitnotes:live:'+scope),'legacy replay');
});
test('legacy source identities match SHA-256 without storing raw plaintext guards',()=>{
 for(const content of ['', '# Unicode é 🐘\n', '# Large\n'+'é'.repeat(1<<19)]){
  const storage=memory(),a=new DraftStorage(storage,owner('a')),scope='u/p/n.md',source=raw(content),key='gitnotes:draft:'+scope;
  storage.setItem(key,source);storage.setItem('gitnotes:live:'+scope,'original pending packet');a.adopt(scope,a.candidates(scope)[0]);
  const expected='sha256:'+createHash('sha256').update(source).digest('hex');let adopted=JSON.parse(a.read('draft',scope));
  assert.equal(adopted.recovery.origin.version,expected);assert.equal(adopted.recovery.replay.version,expected);assert(a.read('draft',scope).length<source.length+1500);
  // Already-written raw guards remain readable and compact on the next write.
  adopted.recovery.origin.version=source;adopted.recovery.replay.version=source;storage.setItem(a.key('draft',scope),JSON.stringify(adopted));
  assert.equal(a.live(scope),'original pending packet');a.write('draft',scope,JSON.parse(a.read('draft',scope)));adopted=JSON.parse(a.read('draft',scope));
  assert.equal(adopted.recovery.origin.version,expected);assert.equal(adopted.recovery.replay.version,expected);
  storage.setItem(key,raw(content+'changed'));assert.equal(a.live(scope),null);
 }
});
test('copied replay stays source-bound without quota duplication and retirement survives reload',()=>{
 const storage=memory(),session=memory(),a=new DraftStorage(storage,owner('a'),session),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 b.write('draft',scope,draft('B'));b.write('live',scope,'pending B');a.adopt(scope,b.candidates(scope)[0]);
 assert.equal(a.read('live',scope),null);assert.equal(a.live(scope),JSON.stringify('pending B'));
 a.remove('live',scope);assert.equal(new DraftStorage(storage,owner('a'),session).live(scope),null);assert.equal(b.live(scope),JSON.stringify('pending B'));
 a.adopt(scope,b.candidates(scope)[0]);b.write('draft',scope,draft('newer B'));b.write('live',scope,'pending newer B');assert.equal(a.live(scope),null);
});
test('retiring a source replay remains durable when session metadata cannot be updated',()=>{
 const storage=memory(),metadata=memory();let fail=false;
 const session={getItem:key=>metadata.getItem(key),setItem:(key,value)=>{if(fail)throw Error('Session quota');metadata.setItem(key,value)}};
 const a=new DraftStorage(storage,owner('a'),session),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 b.write('draft',scope,draft('B'));b.write('live',scope,'pending B');a.adopt(scope,b.candidates(scope)[0]);fail=true;
 a.remove('live',scope);assert.equal(new DraftStorage(storage,owner('a'),session).live(scope),null);assert.equal(b.live(scope),JSON.stringify('pending B'));
});
test('borrowed replay retirement needs no new storage space and failed publication keeps the complete draft',()=>{
 const storage=memory(),a=new DraftStorage(storage,owner('a')),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 b.write('draft',scope,draft('B'));b.write('reconcile',scope,'combined B');b.write('live',scope,'uncertain original B packet');a.adopt(scope,b.candidates(scope)[0]);
 const before=a.read('draft',scope),used=()=>Object.entries(storage).reduce((sum,[key,value])=>sum+key.length+value.length,0),set=storage.setItem;let limit=0;
 Object.defineProperty(storage,'setItem',{value:(key,value)=>{const previous=storage.getItem(key),size=used()+key.length+value.length-(previous===null?0:key.length+previous.length);if(size>limit)throw Error('Browser quota');set(key,value)}});
 assert.throws(()=>a.remove('live',scope),/Browser quota/);
 assert.equal(a.read('draft',scope),before);assert.equal(a.read('reconcile',scope),'combined B');assert.equal(a.live(scope),JSON.stringify('uncertain original B packet'));
 limit=used();a.remove('live',scope);
 const reloaded=new DraftStorage(storage,owner('a'));assert.equal(reloaded.live(scope),null);assert.equal(reloaded.read('reconcile',scope),'combined B');assert.equal(JSON.parse(reloaded.read('draft',scope)).content,'B');assert.equal(b.live(scope),JSON.stringify('uncertain original B packet'));
});
test('equal plaintext candidates retain distinct scratch choices and archive scratch on adoption',()=>{
 const storage=memory(),a=new DraftStorage(storage,owner('a')),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 for(const tab of [a,b])tab.write('draft',scope,draft('same private text'));
 assert.equal(a.candidates(scope).length,1);
 a.write('reconcile',scope,'');assert.equal(a.candidates(scope).length,2);
 b.write('reconcile',scope,'');assert.equal(a.candidates(scope).length,1);
 a.write('reconcile',scope,'scratch A');b.write('reconcile',scope,'scratch B');
 assert.equal(a.candidates(scope).length,2);
 a.adopt(scope,a.candidates(scope).find(candidate=>candidate.owner===b.owner));
 assert.equal(a.read('reconcile',scope),'scratch B');
 assert.deepEqual(new Set(a.candidates(scope).map(candidate=>a.read('reconcile',scope,candidate.owner))),new Set(['scratch A','scratch B']));
 a.write('reconcile',scope,'newer scratch B');
 a.adopt(scope,a.candidates(scope).find(candidate=>a.read('reconcile',scope,candidate.owner)==='scratch A'));
 assert.equal(a.read('reconcile',scope),'scratch A');
 assert.deepEqual(new Set(a.candidates(scope).map(candidate=>a.read('reconcile',scope,candidate.owner))),new Set(['scratch A','scratch B','newer scratch B']));
});
test('dismissing a reviewed candidate does not hide later changes to its scratch alone',()=>{
 const storage=memory(),session=memory(),a=new DraftStorage(storage,owner('a'),session),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 b.write('draft',scope,draft('same private text'));b.write('reconcile',scope,'reviewed scratch');
 a.adopt(scope,a.candidates(scope)[0]);a.resolve(scope);a.remove('draft',scope);
 assert.equal(a.candidates(scope).length,0);
 b.write('reconcile',scope,'newer scratch');
 assert.equal(new DraftStorage(storage,owner('a'),session).candidates(scope).length,1);
});
test('resolution stays durable without writable session metadata before clearing the draft',()=>{
 const storage=memory(),metadata=memory();let fail=false;
 const session={getItem:key=>metadata.getItem(key),setItem:(key,value)=>{if(fail)throw Error('Session quota');metadata.setItem(key,value)}};
 const a=new DraftStorage(storage,owner('a'),session),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 b.write('draft',scope,draft('borrowed B'));b.write('live',scope,'pending B');a.adopt(scope,a.candidates(scope)[0]);a.remove('live',scope);
 fail=true;a.resolve(scope);a.remove('draft',scope);
 const reloaded=new DraftStorage(storage,owner('a'),session);assert.equal(reloaded.candidates(scope).length,0);assert.equal(reloaded.live(scope),null);assert.equal(b.live(scope),JSON.stringify('pending B'));assert(reloaded.resolved(scope));
});
test('failed late adoption publishes neither mixed fields nor a replaced prior owned state',()=>{
 const backing=memory();let fail=false,archived=false;
 const storage=new Proxy(backing,{get:(target,key)=>key==='setItem'?(key,value)=>{
  if(key.startsWith('gitnotes:draft:@'+owner('a')+'.'))archived=true;
  if(fail && (key==='gitnotes:reconcile:@'+owner('a')+'/u/p/n.md' || archived && key==='gitnotes:draft:@'+owner('a')+'/u/p/n.md' && JSON.parse(value).recovery?.schema===1))throw Error('Late publication quota');
  return target.setItem(key,value);
 }:Reflect.get(target,key)});
 const a=new DraftStorage(storage,owner('a')),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 for(const [tab,text] of [[a,'A'],[b,'B']]){tab.write('draft',scope,draft(text));tab.write('reconcile',scope,'scratch '+text);tab.write('live',scope,'pending '+text)}
 const before=a.read('draft',scope),selected=a.candidates(scope).find(candidate=>candidate.owner===b.owner);fail=true;
 assert.throws(()=>a.adopt(scope,selected),/Late publication quota/);assert(archived);
 const reloaded=new DraftStorage(storage,owner('a'));assert.equal(reloaded.read('draft',scope),before);assert.equal(reloaded.read('reconcile',scope),'scratch A');assert.equal(reloaded.live(scope),JSON.stringify('pending A'));
 assert(!reloaded.candidates(scope).some(candidate=>candidate.draft.content==='B'&&candidate.scratch==='scratch A'));
});
test('archiving an unwritable borrowed retry preserves its capability and original packet',()=>{
 const storage=memory(),a=new DraftStorage(storage,owner('a')),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 b.write('draft',scope,draft('B'));b.write('live',scope,'original uncertain B packet');a.adopt(scope,a.candidates(scope)[0]);
 storage.setItem('gitnotes:draft:@00000000-0000-4000-8000-000000000003/'+scope,raw('C'));
 a.adopt(scope,a.candidates(scope).find(candidate=>candidate.draft.content==='C'));
 a.adopt(scope,a.candidates(scope).find(candidate=>candidate.draft.content==='B'));
 assert.equal(a.live(scope),JSON.stringify('original uncertain B packet'));assert.equal(b.live(scope),JSON.stringify('original uncertain B packet'));
});
test('typing publishes the draft object once without reading or parsing the stored record again',()=>{
 const storage=memory(),reads=[],writes=[],parse=JSON.parse,{getItem,setItem}=storage;
 Object.defineProperties(storage,{getItem:{value:key=>{reads.push(key);return getItem(key)}},setItem:{value:(key,value)=>{writes.push(key);setItem(key,value)}}});
 const a=new DraftStorage(storage,owner('a')),scope='u/p/n.md',large='# Novel\n\n'+'Call me Ishmael. '.repeat(1<<16);
 a.write('draft',scope,draft(large));a.write('live',scope,{generation:'g',content:large,update:'',pending:null});
 reads.length=writes.length=0;let parsed=0;
 JSON.parse=(...args)=>{parsed++;return parse(...args)};
 try{for(let i=0;i<20;i++){a.write('draft',scope,draft(large+i));a.write('live',scope,{generation:'g',content:large+i,update:'',pending:null})}}
 finally{JSON.parse=parse}
 assert.deepEqual(reads,[]);assert.equal(parsed,0);assert.equal(writes.length,40);
 assert.equal(JSON.parse(a.read('draft',scope)).content,large+19);assert.equal(JSON.parse(a.live(scope)).content,large+19);
 // Another tab's records are always read from storage.
 const b=new DraftStorage(storage,owner('b'));b.write('draft',scope,draft('B'));
 assert.deepEqual(new Set(a.candidates(scope).map(c=>c.draft.content)),new Set([large+19,'B']));
 b.write('draft',scope,draft('newer B'));
 assert(a.candidates(scope).some(c=>c.draft.content==='newer B'));
 // Removal forgets the kept record, so a reload and this tab agree.
 a.remove('live',scope);a.remove('draft',scope);
 assert.equal(a.read('draft',scope),new DraftStorage(storage,owner('a')).read('draft',scope));
});
// A live session journals the editor's text changes in its live record
// rather than writing a long note whole on every change.
const record=(a,scope,changes,extra={})=>a.write('live',scope,{generation:'g',update:null,pending:null,synced:true,draft:a.version(scope),changes,...extra});
test('a live record’s journal is the draft record’s text, in this tab, after a reload and in other tabs',()=>{
 const storage=memory(),reads=[],{getItem}=storage,a=new DraftStorage(storage,owner('a')),scope='u/p/n.md',large='# Novel\n\n'+'Call me Ishmael. '.repeat(1<<14);
 a.write('draft',scope,draft(large));
 Object.defineProperty(storage,'getItem',{configurable:true,value:key=>{reads.push(key);return getItem(key)}});
 const changes=[];
 for(let i=0;i<20;i++){changes.push([9+i,9+i,'!']);record(a,scope,changes)}
 assert.deepEqual(reads,[],'writing the live record read storage');
 const text='# Novel\n\n'+'!'.repeat(20)+large.slice(9);
 assert.equal(JSON.parse(a.read('draft',scope)).content,text);
 assert.equal(JSON.parse(new DraftStorage(storage,owner('a')).read('draft',scope)).content,text);
 assert.equal(new DraftStorage(storage,owner('b')).candidates(scope)[0].draft.content,text);
 assert.equal(a.liveRecord(scope).content,text);
 // The record's text is unknown while a composition kept it from the editor's.
 record(a,scope,changes,{synced:false});assert.equal(a.liveRecord(scope).content,undefined);
 // A record the previous JavaScript wrote holds its text.
 a.write('live',scope,{generation:'g',content:'old',update:'',pending:null});assert.equal(a.liveRecord(scope).content,'old');assert.equal(JSON.parse(a.read('draft',scope)).content,large);
});
test('a live record continues only the draft record it names, and republishing that record keeps it',()=>{
 const storage=memory(),a=new DraftStorage(storage,owner('a')),scope='u/p/n.md';
 a.write('draft',scope,draft('base'));record(a,scope,[[4,4,' typed']]);
 // Another scratch text republishes the same draft: the live record follows it.
 a.write('reconcile',scope,'scratch');
 assert.equal(JSON.parse(a.read('draft',scope)).content,'base typed');assert.equal(a.liveRecord(scope).content,'base typed');
 assert.deepEqual(JSON.parse(a.read('live',scope)).changes,[]);assert.equal(JSON.parse(a.read('live',scope)).draft,a.version(scope));
 // A draft written whole without the live record naming it: the live record no longer tells its text.
 const before=a.version(scope);a.write('draft',scope,draft('newer text'));
 assert.notEqual(a.version(scope),before);assert.equal(a.liveRecord(scope).content,undefined);assert.equal(JSON.parse(a.read('draft',scope)).content,'newer text');
 // An invalid journal is never folded in.
 record(a,scope,[[5,99,'x']]);assert.equal(JSON.parse(a.read('draft',scope)).content,'newer text');assert.equal(a.liveRecord(scope).content,undefined);
});
test('writing the text it read again keeps the live record, and never drops changes journaled since',()=>{
 const storage=memory(),a=new DraftStorage(storage,owner('a')),scope='u/p/n.md';
 a.write('draft',scope,draft('base'));record(a,scope,[[4,4,' typed']]);
 // Recovery reads the record, then writes the text it shows whole.
 const shown=JSON.parse(a.read('draft',scope)).content;a.write('draft',scope,draft(shown));
 assert.equal(a.liveRecord(scope).content,'base typed');assert.equal(JSON.parse(a.read('live',scope)).draft,a.version(scope));
 // A change journaled after the read is not dropped by writing the older text.
 const read=JSON.parse(a.read('draft',scope)).content;record(a,scope,[[0,0,'> ']]);a.write('draft',scope,draft(read));
 assert.equal(a.liveRecord(scope).content,undefined);
});
test('archiving and adopting carry the journaled text, and a borrowed record keeps telling it only while unchanged',()=>{
 const storage=memory(),a=new DraftStorage(storage,owner('a')),b=new DraftStorage(storage,owner('b')),scope='u/p/n.md';
 b.write('draft',scope,draft('B'));record(b,scope,[[1,1,' journaled']],{pending:{operationId:'b-op'}});
 a.write('draft',scope,draft('A'));record(a,scope,[[1,1,' own']]);
 const candidate=a.candidates(scope).find(c=>c.owner===owner('b'));assert.equal(candidate.draft.content,'B journaled');
 a.adopt(scope,candidate);
 // This tab's journaled work was archived whole, and its live record moved with it.
 const archived=a.candidates(scope).find(c=>c.draft.content==='A own');assert(archived);assert.equal(JSON.parse(archived.packet).changes.length,0);
 assert.equal(JSON.parse(a.read('draft',scope)).content,'B journaled');
 assert.equal(a.liveRecord(scope).content,'B journaled');assert.equal(a.liveRecord(scope).pending.operationId,'b-op');
 // The other tab journals more: the borrowed record no longer tells the adopted text.
 record(b,scope,[[1,1,' journaled'],[0,0,'>']]);
 assert.equal(a.liveRecord(scope).content,'>B journaled');assert.notEqual(a.liveRecord(scope).content,JSON.parse(a.read('draft',scope)).content);
});
test('folding the longest journal of interleaved changes into a long note copies the note once, not per change',()=>{
 // Two people typing in one note break every run of typing: the 64 KiB
 // journal then holds 3,855 changes of one character each.
 const fold=(base,changes)=>{
  const storage=memory(),a=new DraftStorage(storage,owner('a')),scope='u/p/n.md';
  a.write('draft',scope,draft(base));record(a,scope,changes);
  return JSON.parse(new DraftStorage(storage,owner('a')).read('draft',scope)).content;
 };
 const interleaved=(length,count)=>{
  const changes=[];let size=length;
  for(let i=0;i<count;i++){
   const at=Math.floor(size*((i*7919)%1000)/1000);
   changes.push(i%5===4?[at,Math.min(size,at+1),'']:[at,at,i%2?'a':'b']);size+=changes.at(-1)[2].length-(changes.at(-1)[1]-at);
  }
  return changes;
 };
 const slow=(text,changes)=>changes.reduce((value,[from,to,insert])=>value.slice(0,from)+insert+value.slice(to),text);
 const small='Call me Ishmael. '.repeat(6000),smallChanges=interleaved(small.length,3855);
 assert.equal(fold(small,smallChanges),slow(small,smallChanges));
 const novel='Call me Ishmael. Some years ago, never mind how long precisely. '.repeat(19000),changes=interleaved(novel.length,3855);
 // The characters string slices copy, the fold's work whatever the host's
 // speed: the note once into pieces of 8,192, then a piece or two a change.
 const slice=String.prototype.slice;let copied=0,text;
 String.prototype.slice=function(...args){const part=slice.apply(this,args);copied+=part.length;return part};
 try{text=fold(novel,changes)}finally{String.prototype.slice=slice}
 assert.equal(text.length,novel.length+changes.reduce((n,[from,to,insert])=>n+insert.length-(to-from),0));
 assert(copied<novel.length+changes.length*2*16384,'folding copied '+copied+' characters, not '+novel.length+' once');
});
