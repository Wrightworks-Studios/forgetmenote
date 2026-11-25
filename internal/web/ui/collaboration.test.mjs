import test from 'node:test';
import assert from 'node:assert/strict';
import * as Y from 'yjs';
import {encode,decode,fromDelta,LiveSession} from '../../../frontend/collaboration.mjs';

// Text changes {from, to, insert}, each applying to the text the one before it left.
const apply=(text,changes)=>changes.reduce((value,{from,to,insert})=>value.slice(0,from)+insert+value.slice(to),text);
const replace=(text,old,insert)=>{const from=text.indexOf(old);assert(from>=0,old);return {from,to:from+old.length,insert}};
const snapshotOf=(server,extra={})=>({path:'a.md',liveRevision:'g:1',revision:'git',version:'git',pending:true,content:server.getText('markdown').toString(),update:encode(Y.encodeStateAsUpdate(server)),...extra});
const serverWith=text=>{const doc=new Y.Doc();doc.getText('markdown').insert(0,text);return doc};
// An incremental answer: the server's change since the state vector known.
const deltaOf=(server,known,extra={})=>({path:'a.md',liveRevision:'g:1',revision:'git',version:'git',pending:true,incremental:true,content:'',update:encode(Y.encodeStateAsUpdate(server,known)),...extra});

test('a text delta becomes ordered changes, and an embed is refused',()=>{
 assert.deepEqual(fromDelta([{retain:2},{delete:3},{insert:'xy'},{retain:1},{insert:'z'}]),[
  {from:2,to:5,insert:'xy'},
  {from:5,to:5,insert:'z'},
 ]);
 assert.equal(apply('abcdefgh',fromDelta([{retain:2},{delete:3},{insert:'xy'},{retain:1},{insert:'z'}])),'abxyfzgh');
 assert.throws(()=>fromDelta([{retain:1},{insert:{image:'x.png'}},{insert:'a'}]),TypeError);
});

test('local changes apply as given and every other change reaches the editor as changes',async()=>{
 const before='- list\n\nAlpha 😀 paragraph.\n\nRepeated text.\n\nRepeated text.\n\nBeta 😀 paragraph.\n';
 const server=serverWith(before);
 let shown=before,calls=0;
 const session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:changes=>{calls++;shown=apply(shown,changes)},status:()=>{},request:async(_kind,body)=>{Y.applyUpdate(server,decode(body.update));return snapshotOf(server)}});
 try{
  // Two disjoint edits of one input event, as a serializer may produce them.
  const edits=[replace(before,'Beta 😀','Beta 😁'),{from:0,to:1,insert:'*'}];
  session.local(edits);shown=apply(shown,edits);
  assert.equal(calls,0,'the editor saw its own edit again');
  const start=before.indexOf('Alpha 😀');server.transact(()=>{server.getText('markdown').delete(start,'Alpha 😀'.length);server.getText('markdown').insert(start,'Accepted peer 🪶')});
  await session.flush();
  assert.equal(shown,apply(before,edits).replace('Alpha 😀','Accepted peer 🪶'));assert.equal(shown,session.text.toString());
  // Undo is this user's: the peer's text stays, and it arrives as changes.
  session.undo();assert.equal(shown,before.replace('Alpha 😀','Accepted peer 🪶'));
  session.undo(true);assert.equal(shown,apply(before,edits).replace('Alpha 😀','Accepted peer 🪶'));
  await session.flush();assert.equal(server.getText('markdown').toString(),shown);
 }finally{session.stop()}
});
test('updates carry only what the server does not hold yet',async()=>{
 const before='# Manuscript\n\n'+'Call me Ishmael. '.repeat(20000),server=serverWith(before),sent=[];
 const session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:()=>{},status:()=>{},request:async(_kind,body)=>{sent.push(body.update);Y.applyUpdate(server,decode(body.update));return snapshotOf(server)}});
 try{
  session.local([{from:2,to:12,insert:'Moby-Dick'}]);await session.flush();
  const length=session.text.length;session.local([{from:length-2,to:length,insert:'. The end.'}]);await session.flush();
  assert.equal(sent.length,2);
  for(const update of sent)assert(decode(update).length<200,'update of '+decode(update).length+' bytes for a one-word edit');
  assert.equal(server.getText('markdown').toString(),session.text.toString());
  assert.equal(session.text.toString(),'# Moby-Dick\n\n'+'Call me Ishmael. '.repeat(20000).slice(0,-2)+'. The end.');
 }finally{session.stop()}
});
test('a refused incremental update resends the complete state once, under a new operation',async()=>{
 for(const refuseComplete of [false,true]){
  const server=serverWith('base'),requests=[],statuses=[];
  const missing=()=>Object.assign(Error('live operation rejected: Yjs update has missing dependencies; resend complete state'),{status:422,code:'live_operation_rejected'});
  const session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:()=>{},status:(...args)=>statuses.push(args),request:async(_kind,body)=>{
   requests.push(structuredClone(body));
   if(requests.length===1 || refuseComplete)throw missing();
   Y.applyUpdate(server,decode(body.update));return snapshotOf(server);
  }});
  try{
   session.local([{from:4,to:4,insert:' edited'}]);
   if(refuseComplete){
    await assert.rejects(session.flush(),/missing dependencies/);
    assert.equal(requests.length,2);assert.equal(session.closed,true);assert.equal(statuses.at(-1)[2].privateDraft,true);
   }else{
    await session.flush();
    assert.equal(requests.length,2);assert.notEqual(requests[0].operationId,requests[1].operationId);
    // The resent update holds everything: it builds the note on its own.
    const alone=new Y.Doc();Y.applyUpdate(alone,decode(requests[1].update));assert.equal(alone.getText('markdown').toString(),'base edited');
    assert.equal(server.getText('markdown').toString(),'base edited');
    session.local([{from:0,to:0,insert:'>'}]);await session.flush();
    assert(decode(requests[2].update).length<decode(requests[1].update).length,'later updates are incremental again');
   }
  }finally{session.stop()}
 }
});
test('the recovery record follows every change with only what the server does not hold',async()=>{
 const events=new EventTarget(),saved={add:globalThis.addEventListener,remove:globalThis.removeEventListener};
 globalThis.addEventListener=events.addEventListener.bind(events);globalThis.removeEventListener=events.removeEventListener.bind(events);
 const before='base'+' Call me Ishmael.'.repeat(20000),server=serverWith(before),stored=[];
 const session=new LiveSession({snapshot:snapshotOf(server,{pending:false}),persist:value=>stored.push(value),changed:()=>{},status:()=>{},request:()=>new Promise(()=>{})});
 const rebuilt=record=>{const doc=new Y.Doc();Y.applyUpdate(doc,Y.encodeStateAsUpdate(server));Y.applyUpdate(doc,decode(record.update));return doc.getText('markdown').toString()};
 try{
  const painted=()=>new Promise(resolve=>setTimeout(resolve,0));
  for(let i=0;i<50;i++){session.local([{from:4+i,to:4+i,insert:'x'}]);await painted()}
  assert.equal(stored.length,50,'a change was not stored');
  for(const record of stored)assert(JSON.stringify(record).length<1024,'a record of '+JSON.stringify(record).length+' bytes for a '+before.length+'-byte note');
  assert.equal(stored.at(-1).generation,'g');assert.equal(stored.at(-1).synced,true);assert.equal('content' in stored.at(-1),false);
  assert.equal(rebuilt(stored.at(-1)),'base'+'x'.repeat(50)+before.slice(4));
  // Leaving the page stores what the current task changed at once.
  session.local([{from:0,to:0,insert:'>'}]);events.dispatchEvent(new Event('pagehide'));
  assert.equal(stored.length,51);assert.equal(rebuilt(stored[50]),'>base'+'x'.repeat(50)+before.slice(4));
  await painted();assert.equal(stored.length,51,'stored again without new changes');
  session.stop();session.local([{from:0,to:0,insert:'late'}]);events.dispatchEvent(new Event('pagehide'));await painted();assert.equal(stored.length,51);
 }finally{session.stop();globalThis.addEventListener=saved.add;globalThis.removeEventListener=saved.remove}
});
test('leaving the page stores a change whose send waits for the next frame',()=>{
 const events=new EventTarget(),saved={add:globalThis.addEventListener,remove:globalThis.removeEventListener};
 globalThis.addEventListener=events.addEventListener.bind(events);globalThis.removeEventListener=events.removeEventListener.bind(events);
 const server=serverWith('base'),stored=[];
 const session=new LiveSession({snapshot:snapshotOf(server,{pending:false}),persist:value=>stored.push(value),changed:()=>{},status:()=>{},request:()=>new Promise(()=>{})});
 try{
  session.local([{from:4,to:4,insert:'!'}]);assert.equal(stored.length,0);
  events.dispatchEvent(new Event('pagehide'));
  assert.equal(stored.length,1);assert(stored[0].update);
 }finally{session.stop();globalThis.addEventListener=saved.add;globalThis.removeEventListener=saved.remove}
});
test('nothing is stored for recovery once the server holds everything',async()=>{
 const server=serverWith('base'),stored=[];
 const session=new LiveSession({snapshot:snapshotOf(server,{pending:false}),persist:value=>stored.push(value),changed:()=>{},status:()=>{},request:async(_kind,body)=>{Y.applyUpdate(server,decode(body.update));return snapshotOf(server,{pending:false})}});
 try{
  session.local([{from:4,to:4,insert:'!'}]);await session.flush();
  assert(stored.length>=2);assert(stored[0].pending);assert.equal(stored.at(-1),null);
 }finally{session.stop()}
});
test('answers and acknowledgements keep only this tab’s own changes in its record',async()=>{
 const server=serverWith('base'),stored=[],sent=[];let acknowledge;
 const session=new LiveSession({snapshot:snapshotOf(server),persist:value=>stored.push(value),changed:()=>{},status:()=>{},request:(_kind,body)=>{sent.push(body);return new Promise(resolve=>acknowledge=resolve)}});
 const peer=text=>{const known=Y.encodeStateVector(server);server.getText('markdown').insert(server.getText('markdown').length,text);session.receive(deltaOf(server,known));};
 try{
  // Nothing of this tab's own: the server holds everything but its checkpoint.
  peer(' one');
  assert.deepEqual(stored.at(-1),{generation:'g',update:null,pending:null,synced:true});
  session.local([{from:0,to:0,insert:'>'}]);
  const sending=session.flush();assert(stored.at(-1).pending);assert.equal(sent[0].since,'g:1');
  // Peers' changes while this tab's update is in flight are the server's to keep.
  peer(' two');peer(' three');await Promise.resolve();
  assert.equal(stored.at(-1).pending.operationId,sent[0].operationId);
  const known=Y.encodeStateVector(server);Y.applyUpdate(server,decode(sent[0].update));
  acknowledge(deltaOf(server,known,{liveRevision:'g:2'}));await sending;
  assert.deepEqual(stored.at(-1),{generation:'g',update:null,pending:null,synced:true});
  assert.equal(session.text.toString(),'>base one two three');assert.equal(session.snapshot.liveRevision,'g:2');
  assert.equal(session.snapshot.content,'>base one two three');
  // The next update carries only what came after the acknowledged one.
  session.local([{from:1,to:1,insert:'!'}]);const next=session.flush();
  const alone=new Y.Doc();Y.applyUpdate(alone,Y.encodeStateAsUpdate(server));Y.applyUpdate(alone,decode(sent[1].update));
  assert.equal(alone.getText('markdown').toString(),'>!base one two three');assert.equal(Y.parseUpdateMeta(decode(sent[1].update)).from.get(session.doc.clientID),1,'the acknowledged edit was sent again');assert.equal(sent[1].since,'g:2');
  Y.applyUpdate(server,decode(sent[1].update));acknowledge(deltaOf(server,Y.encodeStateVector(server),{liveRevision:'g:3'}));await next;
  // An answer to an older request brings its changes but keeps the newer revision.
  session.receive(deltaOf(server,Y.encodeStateVector(server),{liveRevision:'g:2'}));assert.equal(session.snapshot.liveRevision,'g:3');
 }finally{session.stop()}
});
test('a recovered record rebuilds this tab’s text on the server’s newer state',async()=>{
 const server=serverWith('base'),stored=[];
 const session=new LiveSession({snapshot:snapshotOf(server),persist:value=>stored.push(value),changed:()=>{},status:()=>{},request:()=>new Promise(()=>{})});
 try{
  session.local([{from:4,to:4,insert:' mine'}]);const sending=session.flush();session.local([{from:0,to:0,insert:'> '}]);await new Promise(resolve=>setTimeout(resolve,0));
  // A peer's change the tab never received, then a crash.
  server.getText('markdown').insert(2,'X');
  const sent=[];const recovered=new LiveSession({snapshot:snapshotOf(server),recovered:stored.at(-1),persist:()=>{},changed:()=>{},status:()=>{},request:async(_kind,body)=>{sent.push(body);Y.applyUpdate(server,decode(body.update));return snapshotOf(server)}});
  try{
   assert.equal(recovered.text.toString(),'> baXse mine');
   await recovered.flush();
   assert.deepEqual(sent[0],stored.at(-1).pending,'the operation in flight is retried as it was');
   assert.equal(server.getText('markdown').toString(),'> baXse mine');
  }finally{recovered.stop()}
  sending.catch(()=>{});
 }finally{session.stop()}
});
test('a change is sent at once, and changes made while one is in flight follow it as one',async()=>{
 const server=serverWith('base'),sent=[],answers=[];
 const session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:()=>{},status:()=>{},request:(_kind,body)=>{sent.push(body);return new Promise((resolve,reject)=>answers.push({resolve,reject,body}))}});
 const answer=async(i)=>{const known=Y.encodeStateVector(server);Y.applyUpdate(server,decode(answers[i].body.update));answers[i].resolve(deltaOf(server,known,{liveRevision:'g:'+(i+2)}));await new Promise(resolve=>setTimeout(resolve,5))};
 try{
  session.local([{from:4,to:4,insert:'1'}]);
  assert.equal(sent.length,0,'sent inside the editor’s input event');
  await new Promise(resolve=>setTimeout(resolve,5));assert.equal(sent.length,1,'not sent at once');
  for(const c of '234')session.local([{from:session.text.length,to:session.text.length,insert:c}]);
  await new Promise(resolve=>setTimeout(resolve,5));assert.equal(sent.length,1,'sent while another update was in flight');
  await answer(0);assert.equal(sent.length,2);
  const alone=new Y.Doc();Y.applyUpdate(alone,Y.encodeStateAsUpdate(server));Y.applyUpdate(alone,decode(sent[1].update));assert.equal(alone.getText('markdown').toString(),'base1234');
  assert.equal(Y.parseUpdateMeta(decode(sent[1].update)).from.get(session.doc.clientID),1,'an acknowledged change was sent again');
  await answer(1);assert.equal(server.getText('markdown').toString(),'base1234');
  // After a failure the send is retried on its own schedule, not on each change.
  session.local([{from:0,to:0,insert:'>'}]);await new Promise(resolve=>setTimeout(resolve,5));
  assert.equal(Y.parseUpdateMeta(decode(sent[2].update)).from.get(session.doc.clientID),4,'acknowledged changes were sent again');
  answers[2].reject(Object.assign(Error('offline'),{status:503}));await new Promise(resolve=>setTimeout(resolve,5));
  session.local([{from:0,to:0,insert:'>'}]);await new Promise(resolve=>setTimeout(resolve,5));
  assert.equal(sent.length,3,'a failed send was retried at once');
  const retry=session.flush();assert.deepEqual(sent[3],sent[2],'the retry is not the identical operation');
  await answer(3);assert.equal(sent.length,5);
  await answer(4);await retry;assert.equal(server.getText('markdown').toString(),'>>base1234');
 }finally{session.stop()}
});
test('incremental answers deferred by a composition all apply after it',async()=>{
 const server=serverWith('one'),base=Y.encodeStateVector(server);let shown='one';
 const session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:changes=>shown=apply(shown,changes),status:()=>{},request:()=>new Promise(()=>{})});
 try{
  session.composing=true;
  server.getText('markdown').insert(3,' two');const first=deltaOf(server,base,{liveRevision:'g:2'});
  const known=Y.encodeStateVector(server);server.getText('markdown').insert(7,' three');
  session.receive(first);session.receive(deltaOf(server,known,{liveRevision:'g:3'}));
  assert.equal(shown,'one');
  session.finishComposition();
  assert.equal(shown,'one two three');assert.equal(session.snapshot.liveRevision,'g:3');
 }finally{session.stop()}
});
test('composed text stays private until the composition ends, then remote changes follow it',async()=>{
 const server=serverWith('one\n\ntwo'),requests=[];let shown='one\n\ntwo';
 const session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:changes=>shown=apply(shown,changes),status:()=>{},request:async(_kind,body)=>{requests.push(body);Y.applyUpdate(server,decode(body.update));return snapshotOf(server)}});
 try{
  session.composing=true;
  for(const change of [{from:3,to:3,insert:'き'},{from:3,to:4,insert:'気'}]){session.local([change]);shown=apply(shown,[change])}
  assert.equal(session.text.toString(),'one\n\ntwo');
  await session.flush();assert.equal(requests.length,0);
  server.getText('markdown').insert(server.getText('markdown').length,' remote');
  session.receive(snapshotOf(server));assert.equal(shown,'one気\n\ntwo','a remote change interrupted the composition');
  session.finishComposition();
  assert.equal(session.text.toString(),'one気\n\ntwo remote');assert.equal(shown,session.text.toString());
  await session.flush();assert.equal(server.getText('markdown').toString(),'one気\n\ntwo remote');
 }finally{session.stop()}
});
// The browser can change the editor's text before the editor reads the change
// (Chrome ends an IME composition before its DOM observer reports the last
// text). The session asks the editor for such input before it applies any
// change the editor did not make, so the input joins the note first and the
// change applies over it: as an answer, after a composition, and as undo.
test('the editor reads its unread input before a change it did not make applies',async()=>{
 const before='Alpha.\n\nLocal.\n',typed={from:14,to:14,insert:' kept'};
 const server=serverWith(before),base=Y.encodeStateVector(server),calls=[];
 let shown=before,unread=null,session;
 // As the app wires it: input the editor reads is this user's edit.
 const takeInput=()=>{calls.push('input');if(unread){const changes=[unread];unread=null;shown=apply(shown,changes);session.local(changes)}};
 session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:changes=>{calls.push('changed');shown=apply(shown,changes)},status:()=>{},request:()=>new Promise(()=>{}),takeInput});
 try{
  const start=before.indexOf('Alpha');
  server.transact(()=>{server.getText('markdown').delete(start,5);server.getText('markdown').insert(start,'Accepted AI')});
  // An answer.
  unread=typed;
  session.receive(deltaOf(server,base,{liveRevision:'g:2'}));
  assert.deepEqual(calls,['input','changed']);
  assert.equal(shown,'Accepted AI.\n\nLocal. kept\n');assert.equal(session.text.toString(),shown);
  // An answer deferred by a composition: the composition's last text joins it.
  let known=Y.encodeStateVector(server);server.getText('markdown').insert(0,'# ');
  session.composing=true;session.receive(deltaOf(server,known,{liveRevision:'g:3'}));
  unread={from:shown.length-1,to:shown.length-1,insert:'日本語'};
  session.finishComposition();
  assert.equal(shown,'# Accepted AI.\n\nLocal. kept日本語\n');assert.equal(session.text.toString(),shown);
  // Undo takes the step that ends with the input: redo brings all of it back.
  session.undoManager.stopCapturing();
  session.local([{from:shown.length-1,to:shown.length-1,insert:'!'}]);shown=apply(shown,[{from:shown.length-1,to:shown.length-1,insert:'!'}]);
  unread={from:shown.length-1,to:shown.length-1,insert:'?'};
  session.undo();
  assert.equal(shown,'# Accepted AI.\n\nLocal. kept日本語\n');assert.equal(session.text.toString(),shown);
  session.undo(true);
  assert.equal(shown,'# Accepted AI.\n\nLocal. kept日本語!?\n');assert.equal(session.text.toString(),shown);
 }finally{session.stop()}
});
// While an IME composes, its text is in the editor but not yet in the live
// note: an undo step's offsets are those of the live note, and applied to the
// editor they would change the wrong characters. Undo and Redo wait for the
// composition to end, as every other command does.
test('undo and redo do nothing inside an open IME composition, and the editor stays the live note',async()=>{
 for(const redo of [false,true]) for(const composed of [true,false]){
  const before='Hello world.\n',server=serverWith(before),label=(redo?'redo':'undo')+(composed?' with composed text':' while composing');
  let shown=before,session;
  session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:changes=>shown=apply(shown,changes),status:()=>{},request:async(_kind,body)=>{Y.applyUpdate(server,decode(body.update));return snapshotOf(server)}});
  const type=changes=>{shown=apply(shown,changes);session.local(changes)};
  try{
   type([{from:12,to:12,insert:' Added'}]);await session.flush();
   if(redo){session.undo();assert.equal(shown,before);await session.flush()}
   const live=session.text.toString();
   session.composing=true;
   if(composed)type([{from:0,to:0,insert:'に'}]);
   const editor=shown;
   session.undo(redo);
   assert.equal(session.text.toString(),live,label+' changed the live note');assert.equal(shown,editor,label+' changed the editor');
   session.finishComposition();
   assert.equal(shown,(composed?'に':'')+live,label);assert.equal(shown,session.text.toString(),label);
   await session.flush();assert.equal(server.getText('markdown').toString(),shown,label);
   // Once the composition has ended, Undo and Redo work again.
   session.undoManager.stopCapturing();session.undo(redo);
   assert.equal(shown,session.text.toString(),label);
   if(!composed)assert.equal(shown,redo?'Hello world. Added\n':before,label);
  }finally{session.stop()}
 }
 // A composition's changes still waiting to join the note refuse it too.
 let shown='Hello world.\n';
 const session=new LiveSession({snapshot:snapshotOf(serverWith(shown)),persist:()=>{},changed:changes=>shown=apply(shown,changes),status:()=>{},request:()=>new Promise(()=>{})});
 try{
  session.local([{from:12,to:12,insert:'!'}]);shown=apply(shown,[{from:12,to:12,insert:'!'}]);
  session.composed.push({from:0,to:0,insert:'に'});shown=apply(shown,[{from:0,to:0,insert:'に'}]);
  session.undo();
  assert.equal(session.text.toString(),'Hello world.!\n');assert.equal(shown,'にHello world.!\n');
 }finally{session.stop()}
});
// Input the editor reads first (takeInput) can retire the session: a change
// that no longer matches the live note keeps the text as a private draft.
// Nothing then applies to the retired note or reports over that error.
test('input that retires the session stops the change it preceded',()=>{
 for(const step of ['receive','undo','finishComposition']){
  const before='Hello world.\n',server=serverWith(before),base=Y.encodeStateVector(server),statuses=[];
  let unread=null,session;
  const takeInput=()=>{if(unread){const changes=[unread];unread=null;session.local(changes)}};
  session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:()=>{},status:(...args)=>statuses.push(args),request:()=>new Promise(()=>{}),takeInput});
  try{
   session.local([{from:12,to:12,insert:' Added'}]);
   const live=session.text.toString(),update=Y.encodeStateAsUpdate(session.doc);
   unread={from:40,to:41,insert:'x'};
   if(step==='receive'){
    server.getText('markdown').insert(0,'# ');
    session.receive(deltaOf(server,base,{liveRevision:'g:2'}));
   }else if(step==='undo')session.undo();
   else session.finishComposition();
   assert.equal(session.closed,true,step);
   assert.equal(statuses.length,1,step+' reported over the private draft');assert.equal(statuses[0][2].privateDraft,true,step);
   assert.equal(session.text.toString(),live,step+' changed the retired note');assert.deepEqual(Y.encodeStateAsUpdate(session.doc),update,step);
   assert.equal(session.snapshot.liveRevision,'g:1',step);
  }finally{session.stop()}
 }
});
test('a change that no longer matches the live text retains a private draft without touching shared state',()=>{
 const doc=serverWith('base');
 let error;const session=new LiveSession({snapshot:snapshotOf(doc,{pending:false}),persist:()=>assert.fail('mismatched edit was persisted'),changed:()=>assert.fail('mismatched edit replaced displayed text'),status:(_snapshot,_pending,value)=>error=value,request:()=>assert.fail('mismatched edit was sent')});
 try{
  session.local([{from:2,to:9,insert:'x'}]);assert.equal(session.closed,true);assert.equal(error.privateDraft,true);assert.match(error.message,/private draft/);
  assert.equal(session.text.toString(),'base');assert.deepEqual(Y.encodeStateAsUpdate(session.doc),Y.encodeStateAsUpdate(doc));
 }finally{session.stop()}
});
test('the live size limit is the server’s: a large note is sent and the server’s refusal keeps the draft private',async()=>{
 const doc=serverWith('é'.repeat((1<<19)-1));let error,sent=0;
 const session=new LiveSession({snapshot:snapshotOf(doc,{pending:false}),persist:()=>{},changed:()=>{},status:(_v,_p,value)=>error=value,request:async()=>{sent++;throw Object.assign(Error('live operation rejected: live note must be UTF-8 Markdown of at most 1 MiB'),{status:422,code:'live_operation_rejected'})}});
 try{
  session.local([{from:0,to:0,insert:'界'.repeat(64)}]);assert.equal(session.closed,undefined);
  await assert.rejects(session.flush(),/at most 1 MiB/);
  assert.equal(sent,1);assert.equal(session.closed,true);assert.equal(error.privateDraft,true);
 }finally{session.stop()}
});
test('a typed permanent rejection retires the queue and retains newer text and accepted peer edits',async()=>{
 const doc=serverWith('base\n\npeer');
 const snapshot=()=>snapshotOf(doc,{pending:false});
 const statuses=[],stored=[];let fail,shown='base\n\npeer',calls=0;
 const response=new Promise((_resolve,reject)=>fail=reject);
 const session=new LiveSession({snapshot:snapshot(),persist:v=>stored.push(v),changed:changes=>shown=apply(shown,changes),status:(...v)=>statuses.push(v),request:()=>{calls++;return response}});
 try{
  const first=[{from:0,to:4,insert:'first'}],newer=[{from:5,to:5,insert:' plus newer input'}];
  session.local(first);shown=apply(shown,first);const sending=session.flush();session.local(newer);shown=apply(shown,newer);
  doc.transact(()=>{doc.getText('markdown').delete(6,4);doc.getText('markdown').insert(6,'accepted peer')});
  session.receive(snapshot());assert.equal(shown,'first plus newer input\n\naccepted peer');
  await Promise.resolve();
  const retained=Y.encodeStateAsUpdate(session.doc),count=stored.length;
  fail(Object.assign(Error('deterministic validation rejection'),{status:422,code:'live_operation_rejected'}));
  await assert.rejects(sending,/deterministic/);
  assert.equal(session.closed,true);assert.equal(session.pending,null);assert.equal(session.unsent,false);
  assert.equal(statuses.at(-1)[2].privateDraft,true);assert.equal(stored.length,count,'retired queue was persisted again');
  assert.equal(session.text.toString(),shown);assert.deepEqual(Y.encodeStateAsUpdate(session.doc),retained);
  await session.poll();await assert.rejects(session.flush(),/ended/);assert.equal(calls,1);
 }finally{session.stop()}
});
test('recovered undecodable pending state is retired before replay',()=>{
 const doc=serverWith('base'),snapshot=snapshotOf(doc,{pending:false});
 const recoveredDoc=new Y.Doc();Y.applyUpdate(recoveredDoc,decode(snapshot.update));recoveredDoc.getText('markdown').insert(4,' corrected private draft');
 const pending={path:'a.md',generation:'g',operationId:'old-invalid-operation'};
 for(const update of ['invalid base64!',encode(new Uint8Array((8<<20)+1))]){
  let session;
  try{
   assert.throws(()=>session=new LiveSession({snapshot,recovered:{generation:'g',content:recoveredDoc.getText('markdown').toString(),update:encode(Y.encodeStateAsUpdate(recoveredDoc)),pending:{...pending,update}},persist:()=>assert.fail('invalid recovery was republished'),changed:()=>assert.fail('invalid recovery replaced displayed text'),status:()=>assert.fail('constructor published a callback'),request:()=>assert.fail('invalid recovery was replayed')}),error=>error.privateDraft===true);
  }finally{session?.stop()}
 }
});
test('uncertain failures keep identical pending retries even with a rejection-looking message or code',async()=>{
 for(const failure of [Object.assign(Error('live note must be UTF-8 Markdown of at most 1 MiB'),{status:500}),Object.assign(Error('uncertain owner response'),{status:503,code:'live_operation_rejected'}),Object.assign(Error('unclassified validation'),{status:422})]){
  const doc=serverWith('base'),snapshot=snapshotOf(doc,{pending:false});
  const requests=[];const session=new LiveSession({snapshot,persist:()=>{},changed:()=>{},status:()=>{},request:async(_kind,body)=>{requests.push(structuredClone(body));if(requests.length===1)throw failure;return snapshot}});
  try{
   session.local([{from:0,to:4,insert:'draft'}]);await assert.rejects(session.flush());assert.equal(session.closed,undefined);
   await session.flush();assert.deepEqual(requests[0],requests[1]);assert.equal(requests.length,2);
  }finally{session.stop()}
 }
});
test('local undo preserves concurrent remote text and lost acknowledgement retries exactly',async()=>{
 const seed=serverWith('one\n\ntwo');
 let server=new Y.Doc();Y.applyUpdate(server,Y.encodeStateAsUpdate(seed));
 const requests=[];let lose=true,shown='one\n\ntwo';
 const session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:changes=>shown=apply(shown,changes),status:()=>{},request:async(kind,body)=>{
  requests.push(structuredClone(body));Y.applyUpdate(server,decode(body.update));
  if(lose){lose=false;throw Error('lost acknowledgement')}
  return snapshotOf(server);
 }});
 try {
  const edit=[{from:0,to:3,insert:'ONE'}];session.local(edit);shown=apply(shown,edit);
  await assert.rejects(session.flush(),/lost/);
  await session.flush();
  assert.deepEqual(requests[0],requests[1]);
  server.getText('markdown').insert(server.getText('markdown').length,' remote');
  session.receive(snapshotOf(server));session.undo();
  assert.equal(shown,'one\n\ntwo remote');
 }finally{session.stop()}
});
test('stopped live sessions cannot republish late acknowledgement or failure callbacks',async()=>{
 for(const reject of [false,true]){
  const doc=serverWith('base'),snapshot=snapshotOf(doc,{pending:false});
  let release,fail;const pending=new Promise((resolve,reject)=>{release=resolve;fail=reject});
  const calls=[];
  const session=new LiveSession({snapshot,persist:()=>calls.push('persist'),changed:()=>calls.push('changed'),status:()=>calls.push('status'),request:()=>pending});
  session.local([{from:0,to:4,insert:'draft'}]);const sending=session.flush();session.stop();calls.length=0;
  reject?fail(Error('late failure')):release(snapshot);
  await sending.catch(()=>{});assert.deepEqual(calls,[]);
 }
});
test('a new server generation reports reconciliation before its old session retires',async()=>{
 const snapshot=snapshotOf(serverWith('base'),{pending:false});
 const messages=[];
 const session=new LiveSession({snapshot,persist:()=>{},changed:()=>{},status:(_v,_p,error)=>messages.push(error?.message),request:async()=>({...snapshot,liveRevision:'replacement:1'})});
 try{await session.poll();assert.equal(session.closed,true);assert.match(messages.join(' '),/retained.*reopen and reconcile/)}finally{session.stop()}
});
test('while a request awaits acknowledgement the record holds it and everything this tab added since',async()=>{
 const server=serverWith('base'),snapshot=snapshotOf(server,{pending:false});
 let record,reject;const pending=new Promise((_resolve,fail)=>reject=fail);
 const session=new LiveSession({snapshot,persist:value=>record=value,changed:()=>{},status:()=>{},request:()=>pending});
 try{
  session.local([{from:0,to:4,insert:'first'}]);const sending=session.flush();session.local([{from:5,to:5,insert:' plus newer input'}]);
  await new Promise(resolve=>setTimeout(resolve,0));
  const restored=new Y.Doc();Y.applyUpdate(restored,decode(snapshot.update));Y.applyUpdate(restored,decode(record.update));
  assert.equal(restored.getText('markdown').toString(),'first plus newer input');
  const request=new Y.Doc();Y.applyUpdate(request,decode(snapshot.update));Y.applyUpdate(request,decode(record.pending.update));assert.equal(request.getText('markdown').toString(),'first');
  reject(Error('offline'));await assert.rejects(sending,/offline/);
 }finally{session.stop()}
});

for(const serverPending of [false,true])for(const localState of ['idle','unsent','pending'])test('unchanged poll recovers connection status with '+localState+' edits and checkpoint pending '+serverPending,async()=>{
 const snapshot=snapshotOf(serverWith('base'),{pending:serverPending});
 const statuses=[];let failed=true,release,acknowledge;
 const response=new Promise(resolve=>release=resolve),update=new Promise(resolve=>acknowledge=resolve);
 const session=new LiveSession({snapshot,persist:()=>{},changed:()=>assert.fail('unchanged poll changed content'),status:(...args)=>statuses.push(args),request:async(kind)=>{
  if(failed)throw Error('temporary disconnect');
  return kind==='read'?response:update;
 }});
 try{
  await session.poll();assert.match(statuses.at(-1)[2].message,/temporary disconnect/);
  failed=false;const polling=session.poll();
  let sending;
  if(localState!=='idle')session.local([{from:0,to:4,insert:'retained draft'}]);
  if(localState==='pending')sending=session.flush();
  release({unchanged:true});await polling;
  const recovered=statuses.at(-1);
  assert.equal(recovered[0],snapshot,'healthy unchanged poll did not publish a retained snapshot');
  assert.equal(!!recovered[1],localState!=='idle');assert.equal(recovered[2],undefined);
  assert.equal(session.text.toString(),localState==='idle'?'base':'retained draft');
  if(sending){session.stop();acknowledge(snapshot);await sending}
 }finally{session.stop();acknowledge(snapshot)}
});

test('unchanged polls cannot clear errors after stop or generation invalidation',async()=>{
 for(const invalidGeneration of [false,true]){
  const snapshot=snapshotOf(serverWith('base'),{pending:false});
  const statuses=[];let release;const response=new Promise(resolve=>release=resolve);
  const session=new LiveSession({snapshot,persist:()=>{},changed:()=>{},status:(...args)=>statuses.push(args),request:()=>response});
  try{
   const polling=session.poll();
   if(invalidGeneration)assert.throws(()=>session.receive({...snapshot,liveRevision:'replacement:1'}),/retained/);else session.stop();
   const count=statuses.length;release({unchanged:true});await polling;
   assert.equal(statuses.length,count);
   if(invalidGeneration)assert.match(statuses.at(-1)[2].message,/reopen and reconcile/);
  }finally{session.stop()}
 }
});

// Lets the session's own tasks (a send after a failure, an answer) run.
const tick=()=>new Promise(resolve=>setTimeout(resolve,5));
// A fake server for sends: it applies each update once per operation, as
// LiveApply's receipts do, and answers with what changed since.
function acceptingServer(text){
 const server=serverWith(text),sent=[],operations=new Map();
 const accept=body=>{
  sent.push(structuredClone(body));
  if(!operations.has(body.operationId)){const known=Y.encodeStateVector(server);Y.applyUpdate(server,decode(body.update));operations.set(body.operationId,deltaOf(server,known,{liveRevision:'g:'+(operations.size+2)}))}
  return operations.get(body.operationId);
 };
 return {server,sent,operations,accept};
}
test('a change waiting for its frame is sent at once when the tab is hidden, and once',async()=>{
 const saved={document:globalThis.document,frame:globalThis.requestAnimationFrame},frames=[];
 const page=Object.assign(new EventTarget(),{hidden:false,visibilityState:'visible'});
 globalThis.document=page;globalThis.requestAnimationFrame=callback=>frames.push(callback);
 const {server,sent,accept}=acceptingServer('base');
 const session=new LiveSession({snapshot:snapshotOf(server,{pending:false}),persist:()=>{},changed:()=>{},status:()=>{},request:async(kind,body)=>kind==='update'?accept(body):new Promise(()=>{})});
 try{
  session.local([{from:4,to:4,insert:'!'}]);await tick();
  assert.equal(sent.length,0,'sent before its frame painted');
  // A hidden tab paints no frames: Chrome runs them when it shows again.
  page.hidden=true;page.visibilityState='hidden';page.dispatchEvent(new Event('visibilitychange'));
  assert.equal(sent.length,1,'the change waits for a frame of the hidden tab');
  await tick();assert.equal(server.getText('markdown').toString(),'base!');
  page.hidden=false;page.visibilityState='visible';for(const frame of frames.splice(0))frame();await tick();
  assert.equal(sent.length,1,'the frame sent the change again');
 }finally{session.stop();globalThis.document=saved.document;globalThis.requestAnimationFrame=saved.frame}
});
test('edits that failed to send go at once when the browser comes online, each once and in order',async()=>{
 const events=new EventTarget(),saved={add:globalThis.addEventListener,remove:globalThis.removeEventListener};
 globalThis.addEventListener=events.addEventListener.bind(events);globalThis.removeEventListener=events.removeEventListener.bind(events);
 const {server,sent,accept}=acceptingServer('base');let online=false;
 const session=new LiveSession({snapshot:snapshotOf(server,{pending:false}),persist:()=>{},changed:()=>{},status:()=>{},request:async(kind,body)=>{
  if(!online)throw new TypeError('Failed to fetch');
  return kind==='update'?accept(body):new Promise(()=>{});
 }});
 try{
  session.local([{from:4,to:4,insert:' one'}]);await tick();assert.equal(sent.length,0);
  session.local([{from:8,to:8,insert:' two'}]);await tick();
  // The backoff's first retry is a second away.
  online=true;events.dispatchEvent(new Event('online'));await tick();
  assert.equal(sent.length,2,'the edits did not follow the online event at once');
  assert.equal(server.getText('markdown').toString(),'base one two');
  assert.equal(session.pending,null);assert.equal(session.unsent,false);assert.equal(session.sendFailed,false);
  session.local([{from:0,to:0,insert:'>'}]);await tick();await tick();
  assert.equal(sent.length,3,'sends are no longer held back');assert.equal(server.getText('markdown').toString(),'>base one two');
 }finally{session.stop();globalThis.addEventListener=saved.add;globalThis.removeEventListener=saved.remove}
});
test('edits that failed to send go at once when a poll succeeds after failing, and a lost answer is the same operation',async()=>{
 const {server,sent,operations,accept}=acceptingServer('base');let down=true,lose=false;const reads=[];
 const session=new LiveSession({snapshot:snapshotOf(server,{pending:false}),persist:()=>{},changed:()=>{},status:()=>{},request:async(kind,body)=>{
  if(kind==='read'){reads.push(body);if(down)throw new TypeError('Failed to fetch');return {unchanged:true}}
  // The server stores the update, but its answer is lost.
  if(lose){lose=false;accept(body);throw new TypeError('Failed to fetch')}
  if(down)throw new TypeError('Failed to fetch');
  return accept(body);
 }});
 try{
  lose=true;session.local([{from:4,to:4,insert:' one'}]);await tick();
  assert.equal(sent.length,1);assert.equal(server.getText('markdown').toString(),'base one');
  session.local([{from:8,to:8,insert:' two'}]);await tick();
  // A failed send is not the poll's to retry: the poll reads.
  await session.poll();assert.equal(reads.length,1);assert.equal(sent.length,1);
  down=false;await session.poll();await tick();await tick();
  assert.equal(sent.length,3,'the edits did not follow the first successful poll');
  assert.deepEqual(sent[1],sent[0],'the retry is not the same operation');
  assert.equal(operations.size,2);assert.equal(server.getText('markdown').toString(),'base one two');
 }finally{session.stop()}
});
test('a checkpoint asks for only what changed after the revision the tab holds',async()=>{
 const server=serverWith('base'),requests=[];
 const session=new LiveSession({snapshot:snapshotOf(server),persist:()=>{},changed:()=>{},status:()=>{},request:async(kind,body)=>{requests.push([kind,body]);return deltaOf(server,Y.encodeStateVector(server),{liveRevision:'g:2',pending:false})}});
 try{
  await session.checkpoint();
  assert.deepEqual(requests,[['checkpoint',{path:'a.md',since:'g:1'}]]);
  assert.equal(session.snapshot.pending,false);assert.equal(session.snapshot.content,'base');
 }finally{session.stop()}
});
