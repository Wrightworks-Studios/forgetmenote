import {TextPieces} from './text-pieces.mjs';
const prefix = kind => 'gitnotes:'+kind+':';
const keyFor = (kind,scope,owner) => prefix(kind)+(owner?'@'+owner+'/':'')+scope;
function location(key,kind){
 if(!key.startsWith(prefix(kind)))return null;
 const rest=key.slice(prefix(kind).length);
 const owned=/^@([a-f0-9-]{36}(?:\.[a-f0-9-]{36})?)\//.exec(rest);
 // The bootstrap account's legacy scope begins with @local-admin, not a
 // tab UUID. Ordinary account IDs cannot begin with @.
 return owned?{owner:owned[1],scope:rest.slice(owned[0].length)}:{owner:'',scope:rest};
}
const equivalent=(a,b)=>a.draft.content===b.draft.content&&a.draft.revision===b.draft.revision&&a.draft.version===b.draft.version&&!!a.draft.private===!!b.draft.private&&a.scratch===b.scratch&&a.packet===b.packet;

// Legacy records lack a candidate UUID. SHA-256 binds their exact UTF-8 snapshot
// without repeating large private text in source guards. This stays synchronous
// so reading or publishing one browser-storage record needs no asynchronous gap.
const sha256Rounds=new Uint32Array([
 0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
 0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
 0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
 0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
 0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
 0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
 0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
 0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2,
]);
function sourceVersion(raw){
 if(raw===null)return null;
 try{const version=JSON.parse(raw)?.candidateVersion;if(version)return version}catch{}
 const bytes=new TextEncoder().encode(raw),padded=new Uint8Array(Math.ceil((bytes.length+9)/64)*64),words=new Uint32Array(64);
 padded.set(bytes);padded[bytes.length]=0x80;
 const view=new DataView(padded.buffer);view.setUint32(padded.length-8,Math.floor(bytes.length/0x20000000));view.setUint32(padded.length-4,bytes.length*8);
 const hash=new Uint32Array([0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19]),rotate=(x,n)=>(x>>>n)|(x<<(32-n));
 for(let offset=0;offset<padded.length;offset+=64){
  for(let i=0;i<16;i++)words[i]=view.getUint32(offset+i*4);
  for(let i=16;i<64;i++){
   const x=words[i-15],y=words[i-2],s0=rotate(x,7)^rotate(x,18)^(x>>>3),s1=rotate(y,17)^rotate(y,19)^(y>>>10);
   words[i]=words[i-16]+s0+words[i-7]+s1;
  }
  let [a,b,c,d,e,f,g,h]=hash;
  for(let i=0;i<64;i++){
   const t1=(h+(rotate(e,6)^rotate(e,11)^rotate(e,25))+((e&f)^(~e&g))+sha256Rounds[i]+words[i])|0;
   const t2=((rotate(a,2)^rotate(a,13)^rotate(a,22))+((a&b)^(a&c)^(b&c)))|0;
   h=g;g=f;f=e;e=(d+t1)|0;d=c;c=b;b=a;a=(t1+t2)|0;
  }
  const block=[a,b,c,d,e,f,g,h];for(let i=0;i<8;i++)hash[i]+=block[i];
 }
 return 'sha256:'+Array.from(hash,n=>n.toString(16).padStart(8,'0')).join('');
}
const compactVersion=version=>typeof version==='string' && version.trimStart().startsWith('{')?sourceVersion(version):version;
// A live record of this JavaScript names the draft record it continues
// (`draft`, that record's candidateVersion) and journals the text changes
// made after it was written whole (`changes`, [from, to, insert] in UTF-16
// units, each applying to the text the one before it left), so typing never
// writes a long note whole. Journaled text is the record's text. Records of
// the previous JavaScript held the whole text instead, as `content`.
// The fold copies only the pieces each change touches (TextPieces): slicing
// the whole text again for each change took 1.4 s for the 3,855 changes a
// journal can hold in a 1.2 MB note, when two people type in it.
function journaled(text,changes){
 if(!Array.isArray(changes))return null;
 const pieces=new TextPieces(text);
 try{pieces.apply(changes.map(change=>{const [from,to,insert]=Array.isArray(change)?change:[];return {from,to,insert}}))}
 catch{return null}
 return pieces.text;
}
const parse=raw=>{try{return JSON.parse(raw)}catch{return null}};
const compactReference=ref=>ref && {...ref,version:compactVersion(ref.version)};
const matchesSource=(raw,version)=>raw!==null && (raw===version || sourceVersion(raw)===compactVersion(version));

// One draft record publishes plaintext and its complete recovery metadata.
// Large replay bytes stay separate. New branches use a fresh owned replay key;
// borrowed references are version-guarded and never require copying those bytes.
export class DraftStorage {
 constructor(storage,owner,session){
  Object.assign(this,{storage,owner});this.legacy={};
  // This tab is the only writer of its own records (its owner identity is
  // locked), so it keeps each one it reads or publishes instead of reading
  // and parsing the whole draft from storage again on every keystroke.
  this.records=new Map();
  // Read old session metadata only while importing an old draft record.
  try{this.legacy=JSON.parse(session?.getItem('gitnotes:recovery:'+owner))||{}}catch{}
 }
 // A record as stored, with its live record's journal folded in (fold).
 state(scope,owner=this.owner){return this.fold(this.entry(scope,owner))}
 // A record as stored; a tab's own live record follows it on every change,
 // so writing that never reads the journal back.
 entry(scope,owner=this.owner){
  const key=keyFor('draft',scope,owner),own=owner===this.owner;
  if(own && this.records.has(key))return this.records.get(key);
  const raw=this.storage.getItem(key);let value;try{value=JSON.parse(raw)}catch{}
  value=value||{content:null,revision:''};
  let state;
  if(value.recovery?.schema===1)state={key,raw,value,recovery:value.recovery};
  else {
   const origin=own?this.legacy.origins?.[scope]:null;
   state={key,raw,value,recovery:{schema:1,scratch:this.storage.getItem(keyFor('reconcile',scope,owner)),liveKey:keyFor('live',scope,owner),
    replay:origin&&!origin.replayRetired?{key:keyFor('live',scope,origin.owner),guardKey:origin.key,version:origin.version}:null,
    origin:origin||null,ignored:own?{...this.legacy.ignored}:{}}};
  }
  state.stored={raw,value};
  if(own)this.records.set(key,state);
  return state;
 }
 fold(state){
  const packet=this.storage.getItem(state.recovery.liveKey),{stored}=state;
  if(packet===state.folded)return state;
  state.folded=packet;state.raw=stored.raw;state.value=stored.value;
  const live=packet && parse(packet);
  if(typeof stored.value.content==='string' && live?.draft && live.draft===stored.value.candidateVersion && live.changes?.length){
   const content=journaled(stored.value.content,live.changes);
   if(content!==null){state.value={...stored.value,content};state.raw=JSON.stringify(state.value)}
  }
  return state;
 }
 // The candidateVersion of this tab's record, which its live record names.
 version(scope){const {value}=this.entry(scope).stored;return typeof value.content==='string' && value.candidateVersion || null}
 publish(scope,value,recovery,owner=this.owner,version=crypto.randomUUID()){
  recovery={...recovery,origin:compactReference(recovery.origin),replay:compactReference(recovery.replay),
   ignored:Object.fromEntries(Object.entries(recovery.ignored).map(([key,value])=>[key,typeof value==='string'?compactVersion(value):compactReference(value)]))};
  const record={...value,candidateVersion:version,savedAt:Date.now(),recovery},raw=JSON.stringify(record),key=keyFor('draft',scope,owner);
  const previous=owner===this.owner && this.records.get(key);
  this.storage.setItem(key,raw);
  if(owner===this.owner)this.records.set(key,{key,raw,value:record,recovery,stored:{raw,value:record}});
  // The text read last, journal folded in, published again: its live record,
  // unchanged since, continues the new version with nothing left to journal.
  if(previous?.folded && typeof record.content==='string' && (value===previous.value || value.content===previous.value.content))
   this.rebind(recovery.liveKey,previous.folded,previous.stored.value.candidateVersion,record.candidateVersion);
  return record;
 }
 rebind(liveKey,folded,from,to){
  const raw=this.storage.getItem(liveKey),live=raw===folded && parse(raw);
  if(from && live?.draft===from)this.storage.setItem(liveKey,JSON.stringify({...live,draft:to,changes:[]}));
 }
 key(kind,scope){return kind==='live'?this.state(scope).recovery.liveKey:keyFor(kind,scope,this.owner)}
 read(kind,scope,owner=this.owner){
  const state=this.state(scope,owner);
  if(kind==='draft')return typeof state.value.content==='string'?state.raw:null;
  if(kind==='reconcile')return state.recovery.scratch;
  return this.storage.getItem(state.recovery.liveKey);
 }
 // A draft or live record is an object; a reconcile scratch is its text.
 write(kind,scope,value){
  if(kind==='live'){this.storage.setItem(this.entry(scope).recovery.liveKey,JSON.stringify(value));return}
  if(kind==='draft'){this.publish(scope,value,this.entry(scope).recovery);return}
  const state=this.state(scope);
  this.publish(scope,state.value,{...state.recovery,scratch:value});
 }
 remove(kind,scope){
  const state=this.state(scope),r=state.recovery;
  if(kind==='live'){
   if(!r.replay && this.storage.getItem(r.liveKey)===null)return;
   // The record retires replay before optional byte cleanup. Replacing it
   // needs no extra marker key, even when a borrowed packet fills storage.
   this.publish(scope,state.value,{...r,replay:null,liveKey:keyFor('live',scope,this.owner+'.'+crypto.randomUUID())});
   try{this.storage.removeItem(r.liveKey)}catch{}
  }else if(kind==='reconcile'){
   if(r.scratch!==null)this.publish(scope,state.value,{...r,scratch:null});
  }else if(r.resolved || r.origin || Object.keys(r.ignored).length || r.scratch!==null || r.liveKey!==keyFor('live',scope,this.owner)){
   // Keep a retired branch pointer from falling back to an old legacy key.
   this.publish(scope,{content:null,revision:''},r);
  }else {this.storage.removeItem(state.key);this.records.delete(state.key)}
 }
 packet(state){
  if(typeof state.value.content!=='string')return {packet:null,replay:null};
  const own=this.storage.getItem(state.recovery.liveKey);
  if(own!==null)return own==='null'?{packet:null,replay:null}:{packet:own,replay:{key:state.recovery.liveKey,guardKey:state.key,version:sourceVersion(state.raw)}};
  const ref=state.recovery.replay;if(!ref)return {packet:null,replay:null};
  if(!matchesSource(this.storage.getItem(ref.guardKey),ref.version))return {packet:null,replay:null};
  const packet=this.storage.getItem(ref.key);return packet===null||packet==='null'?{packet:null,replay:null}:{packet,replay:ref};
 }
 live(scope){return this.packet(this.state(scope)).packet}
 // The live record recovery replays, with `content`, the text it holds: a
 // record of the previous JavaScript holds it, and a record of this one
 // journals it after the draft record it names, when that is still the
 // record it was written for. Its text is unknown otherwise, and while an IME
 // composition kept the record's text from the editor's.
 liveRecord(scope){
  const state=this.state(scope),{packet,replay}=this.packet(state),live=packet && parse(packet);
  if(!live || typeof live!=='object' || !('draft' in live))return live||null;
  let content;
  if(live.synced!==false){
   const source=replay.guardKey===state.key?state.stored.value:parse(this.storage.getItem(replay.guardKey));
   if(typeof source?.content==='string' && live.draft===source.candidateVersion)content=journaled(source.content,live.changes)??undefined;
  }
  return {...live,content};
 }
 resolved(scope){const state=this.state(scope);return !!state.recovery.resolved&&typeof state.value.content!=='string'}
 candidates(scope){
  const result=[],ignored=this.state(scope).recovery.ignored;
  for(const key of Object.keys(this.storage)){
   const at=location(key,'draft');if(!at||at.scope!==scope)continue;
   const state=this.state(scope,at.owner),draft=state.value,scratch=state.recovery.scratch,dismissed=ignored[key];
   if(typeof draft.content!=='string'||typeof draft.revision!=='string')continue;
   if(dismissed && matchesSource(state.raw,dismissed?.version??dismissed) && (dismissed?.scratch??null)===scratch)continue;
   result.push({...at,key,raw:state.raw,draft,scratch,...this.packet(state)});
  }
  result.sort((a,b)=>(b.owner===this.owner)-(a.owner===this.owner)||(b.draft.savedAt||0)-(a.draft.savedAt||0)||a.key.localeCompare(b.key));
  return result.filter((candidate,index)=>!result.slice(0,index).some(other=>equivalent(other,candidate)));
 }
 paths(scopePrefix){
  const paths=new Set();
  for(const key of Object.keys(this.storage)){const at=location(key,'draft');if(at?.scope.startsWith(scopePrefix)&&typeof this.state(at.scope,at.owner).value.content==='string')paths.add(at.scope.slice(scopePrefix.length))}
  return [...paths];
 }
 archive(scope){
  const state=this.state(scope);if(typeof state.value.content!=='string')return;
  const archive=this.owner+'.'+crypto.randomUUID(),liveKey=keyFor('live',scope,archive),own=this.storage.getItem(state.recovery.liveKey);
  // Publish the archive only after its owned packet is complete. A failed
  // write leaves at most an unreachable packet, never a partial candidate.
  const live=own!==null && parse(own),draft=crypto.randomUUID();
  if(own!==null)this.storage.setItem(liveKey,live?.draft && live.draft===state.stored.value.candidateVersion?JSON.stringify({...live,draft,changes:[]}):own);
  this.publish(scope,state.value,{...state.recovery,liveKey,origin:null,ignored:{}},archive,draft);
 }
 detach(scope){this.archive(scope);const state=this.state(scope);this.publish(scope,{content:null,revision:''},{...state.recovery,resolved:true,scratch:null,origin:null,replay:null,liveKey:keyFor('live',scope,this.owner+'.'+crypto.randomUUID())})}
 adopt(scope,candidate,raw=candidate.raw){
  if(candidate.owner===this.owner)return;
  const state=this.state(scope),old={draft:state.value,scratch:state.recovery.scratch,...this.packet(state)};
  if(typeof old.draft.content==='string' && !equivalent(old,{...candidate,draft:JSON.parse(raw)}))this.archive(scope);
  const origin={key:candidate.key,owner:candidate.owner,version:sourceVersion(candidate.raw),scratch:candidate.scratch};
  // This is the only publication point. No rollback or session-storage write
  // is needed to keep the previous complete owned candidate on failure.
  this.publish(scope,JSON.parse(raw),{schema:1,scratch:candidate.scratch,origin,ignored:state.recovery.ignored,
   liveKey:keyFor('live',scope,this.owner+'.'+crypto.randomUUID()),replay:candidate.replay});
 }
 resolve(scope,next){
  const state=this.state(scope),origin=state.recovery.origin,ignored={...state.recovery.ignored};
  // A foreign live tab remains its record's sole owner. An explicit Save or
  // Discard only dismisses that reviewed version in this tab, not in its owner.
  if(origin)ignored[origin.key]={version:origin.version,scratch:origin.scratch??null};
  this.publish(scope,next===undefined?state.value:next||{content:null,revision:''},{schema:1,resolved:true,scratch:null,origin:null,ignored,replay:null,liveKey:keyFor('live',scope,this.owner+'.'+crypto.randomUUID())});
  // Publication above durably records resolution before any cleanup or UI
  // success. Cleanup is optional; failures cannot undo the resolved choice.
  try{if(origin && (!origin.owner || origin.owner.startsWith(this.owner+'.'))){
   const raw=this.storage.getItem(origin.key);
   const referenced=Object.keys(this.storage).some(key=>{try{return JSON.parse(this.storage.getItem(key))?.recovery?.replay?.guardKey===origin.key}catch{return false}});
   if(!referenced && matchesSource(raw,origin.version) && this.read('reconcile',scope,origin.owner)===(origin.scratch??null)){
    try{this.storage.removeItem(origin.key)}catch{}
   }
  }}catch{}
 }
}

export async function openDraftStorage(){
 let preferred;try{preferred=sessionStorage.getItem('gitnotes:tab')}catch{}
 if(!/^[a-f0-9-]{36}$/.test(preferred||''))preferred=crypto.randomUUID();
 let owner=crypto.randomUUID();
 if(navigator.locks?.request){
  const claim=id=>new Promise(resolve=>{
   navigator.locks.request('gitnotes:tab:'+id,{ifAvailable:true},lock=>{
    resolve(lock?id:null);
    // The browser releases this lock when this document actually ends.
    if(lock)return new Promise(()=>{});
   }).catch(()=>resolve(null));
  });
  owner=await claim(preferred)||await claim(owner)||crypto.randomUUID();
 }
 // Duplicated tabs can inherit sessionStorage. Only a live exclusive lock
 // permits reuse; without locks, keep the old owner as a recovery candidate.
 try{sessionStorage.setItem('gitnotes:tab',owner)}catch{}
 return new DraftStorage(localStorage,owner,sessionStorage);
}
