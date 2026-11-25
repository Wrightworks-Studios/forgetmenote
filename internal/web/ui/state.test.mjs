import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createApplicationState,requestSequence,poll} from './state.mjs';

test('application selection changes atomically and unsubscribes without dropping drafts',()=>{
 const note={path:'A.md'},state=createApplicationState({notebook:null,account:null,note,collection:'notes'}),seen=[];
 const stop=state.subscribe((current,previous)=>seen.push([current,previous]));
 const before=state.read();state.update({notebook:{id:'first'},account:{id:'alice'}});
 assert.equal(seen.length,1);assert.equal(seen[0][1],before);
 assert.equal(state.note(),note);assert.equal(state.notebook().id,'first');assert.equal(state.account().id,'alice');
 assert.throws(()=>{state.read().collection='archives';},TypeError);
 state.update({note});assert.equal(seen.length,1);
 stop();state.update({collection:'drafts'});assert.equal(seen.length,1);
});

test('independent request lifetimes reject superseded responses',()=>{
 const navigation=requestSequence(),preview=requestSequence(),first=navigation.next(),render=preview.next();
 assert.ok(navigation.owns(first));navigation.next();assert.ok(!navigation.owns(first));assert.ok(preview.owns(render));
});

test('a superseded request aborts the fetch that passed its signal',()=>{
 const navigation=requestSequence(),preview=requestSequence();
 navigation.next();const first=navigation.signal();
 assert.equal(navigation.signal(),first,'one signal per request');
 const render=preview.begin(),other=preview.signal();
 assert.equal(first.aborted,false);
 navigation.next();
 assert.equal(first.aborted,true,'the next request cancels the superseded fetch');
 assert.equal(navigation.signal().aborted,false);
 assert.equal(other.aborted,false,'independent sequences keep their requests');assert.ok(render());
 preview.begin();assert.equal(other.aborted,true,'begin supersedes like next');
});

test('request ownership also follows the captured account or notebook authority',()=>{
 const list=requestSequence(),metadata=requestSequence();let notebook='first';
 const owns=list.begin(()=>notebook==='first'),other=metadata.begin();
 assert.ok(owns());notebook='second';assert.ok(!owns());assert.ok(other());
 const replacement=list.begin(()=>notebook==='second');assert.ok(replacement());
 list.next();assert.ok(!replacement());assert.ok(other());
});

test('polling follows its owner, never overlaps, pauses while hidden and resumes when visible',async()=>{
 const before=globalThis.document,listeners=new Set(),page={hidden:false,addEventListener:(_,f)=>listeners.add(f),removeEventListener:(_,f)=>listeners.delete(f)};
 globalThis.document=page;
 const settle=()=>new Promise(resolve=>setTimeout(resolve,20));
 try {
  let owner=true,ticks=0,inFlight=0,overlap=false,release;
  const loop=poll(async()=>{ticks++;overlap||=++inFlight>1;await new Promise(resolve=>release=resolve);inFlight--;},{every:1,active:()=>owner});
  await settle();assert.equal(ticks,1);release();await settle();assert.equal(ticks,2,'reschedules after the previous tick settles');
  for(const listener of listeners)listener();await settle();assert.equal(ticks,2,'a running tick is not repeated');
  assert.equal(overlap,false);
  page.hidden=true;release();await settle();assert.equal(ticks,2,'hidden tabs pause');
  page.hidden=false;for(const listener of listeners)listener();await settle();assert.equal(ticks,3,'visible again runs at once');
  owner=false;release();await settle();assert.equal(ticks,3);assert.equal(listeners.size,0,'a superseded owner stops the loop');
  await loop.done;
  let once=0;await poll(()=>{once++;return false;},{every:1}).done;assert.equal(once,1,'a tick returning false ends the loop');
  const stopped=poll(()=>assert.fail('stopped loop ran'),{every:1});stopped.stop();await settle();await stopped.done;
  await assert.rejects(poll(()=>{throw Error('tick failed');},{every:1}).done,/tick failed/);
  let hidden=0;page.hidden=true;await poll(()=>++hidden<2,{every:1,whileHidden:true}).done;assert.equal(hidden,2,'explicit hidden polling continues');
 } finally {globalThis.document=before;}
});
