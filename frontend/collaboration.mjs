import * as Y from 'yjs';
import {poll} from '../internal/web/ui/state.mjs';

// Transaction origins: accepted server state, and this user's own edits (the
// only ones the undo manager tracks besides its own undo and redo).
const remote = Symbol('remote');
const local = Symbol('local');
// Recovered browser state is untrusted local data; bound what it may decode.
const maxLiveStateBytes = 8 << 20;
export const encode = data => {
  let binary='';for(let i=0;i<data.length;i+=32768)binary+=String.fromCharCode(...data.subarray(i,i+32768));
  return btoa(binary);
};
export const decode = value => Uint8Array.from(atob(value), c=>c.charCodeAt(0));

function decodeRecoveredUpdate(update) {
  const bytes=decode(update);
  if(bytes.length>maxLiveStateBytes)throw new Error('Recovered live state exceeds 8 MiB. Compare and reconcile the retained private draft.');
  return bytes;
}
// The server refuses an update whose structures build on state it does not
// hold and asks for the complete state instead (workspace LiveApply).
const missingDependencies = error => error.status===422 && error.code==='live_operation_rejected' && /missing dependencies/.test(error.message||'');
// The live revision's sequence number within its generation.
const sequence = revision => Number(revision.split(':')[1]);
// The clock each Yjs client reaches in an update, whether it holds a
// client's structures from the start or only those after a state vector.
const clocks = update => Y.parseUpdateMeta(update).to;
// Runs fn once the browser has painted what the current task changed, so a
// keystroke shows before its change is stored or sent.
const afterPaint = fn => {
  if(globalThis.requestAnimationFrame && !globalThis.document?.hidden)requestAnimationFrame(()=>setTimeout(fn,0));
  else setTimeout(fn,0);
};
// A Yjs text delta ({retain}, {insert}, {delete}) as ordered changes, each
// applying to the text the one before it left. Markdown text holds only
// characters, so an embed (a non-string insert) throws rather than shifting
// every later offset.
export function fromDelta(delta) {
  const changes = [];
  let at = 0;
  for (const op of delta) {
    if (op.retain) at += op.retain;
    else if (op.delete) changes.push({from: at, to: at + op.delete, insert: ''});
    else if (op.insert !== undefined) {
      if (typeof op.insert !== 'string') throw new TypeError('A Markdown text delta cannot insert an embed');
      const last = changes.at(-1);
      if (last && last.from === at && !last.insert) last.insert = op.insert;
      else changes.push({from: at, to: at, insert: op.insert});
      at += op.insert.length;
    }
  }
  return changes;
}
// Failed requests are retried 1, 2, 4… seconds apart, up to 30 s.
const backoff = failures => Math.min(30000,1000*2**Math.min(failures-1,5));

// One live Markdown document. The editor's own edits go in as text changes
// (local), and every change it did not make (peers, agents, undo) comes back
// through changed(changes), each {from, to, insert} applying to the text the
// one before it left. Live size limits are the server's: an update it refuses
// retires the session and keeps the text as a private draft.
//
// The wire is incremental: requests name the live revision this tab holds
// (`since`), and the server answers with only the Yjs update after it, or
// with the whole note when it cannot (a first read, a new generation, a
// revision it no longer keeps, or a server that predates this). A change is
// sent once it has painted when no send is in flight; changes made while one
// is coalesce into the next. A failed send is retried on its own backoff,
// and at once when the browser comes online or a poll succeeds after
// failing: the server can be reached again.
//
// takeInput() asks the editor for text the browser changed that it has not
// read yet, which it then reports as this user's edit (local). The session
// asks before it applies any change the editor did not make, so this user's
// text is in the note first and the change applies over it: a change
// dispatched to the editor over unread input would throw that input away.
export class LiveSession {
  constructor({snapshot,request,changed,status,persist,recovered,abort,takeInput}) {
    Object.assign(this,{request,changed,status,persist,abort,takeInput});
    this.doc=new Y.Doc();this.text=this.doc.getText('markdown');
    this.generation=snapshot.liveRevision.split(':')[0];this.initial=snapshot;this.snapshot=snapshot;
    const initial=decode(snapshot.update);
    Y.applyUpdate(this.doc,initial,remote);
    // What the server is known to hold, by client: updates are encoded
    // against it. It only grows within a generation.
    this.acknowledged=clocks(initial);
    this.undoManager=new Y.UndoManager(this.text,{trackedOrigins:new Set([local])});
    this.composed=[];this.deferred=[];
    const recovery=recovered?.generation===this.generation ? recovered : null;
    this.pending=recovery?.pending ?? null;
    try{
      // A retained retry must decode before any replay of it starts.
      if(this.pending)Y.encodeStateVectorFromUpdate(decodeRecoveredUpdate(this.pending.update));
      if(recovery?.update)Y.applyUpdate(this.doc,decodeRecoveredUpdate(recovery.update));
    }catch(error){
      error.privateDraft=true;this.undoManager.destroy();this.doc.destroy();this.stop();throw error;
    }
    // A send stores the record with its update; a change that waits for one
    // in flight stores it on its own.
    this.doc.on('update',(_update,origin)=>{if(origin!==remote){this.unsent=true;if(!this.schedule())this.save()}});
    this.text.observe((event,transaction)=>{if(transaction.origin!==local && !this.closed)this.changed(fromDelta(event.delta))});
    if(this.pending || recovery?.update){this.unsent=true;this.schedule()}
    // The recovery record is small and follows every change; leaving or
    // hiding the page writes one a change has not reached yet.
    this.leave=()=>{if(this.saving || this.unsent)this.store()};
    // A hidden tab paints no frames, so a change waiting for one is sent now.
    this.hidden=()=>{if(globalThis.document?.visibilityState==='hidden'){this.send();this.leave()}};
    this.online=()=>{this.retry();this.loop?.wake()};
    globalThis.addEventListener?.('pagehide',this.leave);
    globalThis.addEventListener?.('online',this.online);
    globalThis.document?.addEventListener?.('visibilitychange',this.hidden);
    // A long poll waits on the server until the note changes, so the next
    // one starts at once; hidden tabs poll more slowly, and failures back
    // off up to 30 s.
    this.loop=poll(()=>this.poll(),{first:750,whileHidden:true,active:()=>!this.closed,
      every:()=>this.pollFailures?backoff(this.pollFailures):globalThis.document?.hidden?5000:this.composing?250:50});
  }
  // The recovery record holds only what the server does not: the update
  // since the state it acknowledged and the operation in flight, for the
  // generation they apply to, or null when the server holds everything and
  // has checkpointed it. `synced` says whether the record's text is the
  // editor's, which an open IME composition keeps private. The editor's own
  // text is the draft record's (internal/web/ui/app.mjs journals it).
  store(){
    this.saving=false;
    if(this.closed)return;
    const synced=!this.composing && !this.composed.length;
    if(!this.unsent && !this.pending && synced && !this.snapshot.pending)return this.persist(null);
    const update=this.unsent?encode(Y.encodeStateAsUpdate(this.doc,Y.encodeStateVector(this.acknowledged))):this.pending?.update ?? null;
    this.persist({generation:this.generation,update,pending:this.pending,synced});
  }
  // Stores the record once the current task's changes have painted.
  save(){if(!this.saving && !this.closed){this.saving=true;afterPaint(()=>{if(this.saving)this.store()})}}
  // Applies the editor's changes, in order, as one undoable transaction.
  // Text inside an open IME composition stays private: its changes wait for
  // finishComposition().
  local(changes){
    if(this.closed || !changes?.length)return;
    if(this.composing){for(const change of changes)this.composed.push(change);this.save();return}
    let length=this.text.length;
    for(const {from,to,insert} of changes){
      if(!Number.isInteger(from) || !Number.isInteger(to) || from<0 || to<from || to>length || typeof insert!=='string'){
        this.retainPrivate(new Error('This edit no longer matches the live note. Your text is retained as a private draft; compare and reconcile it before sharing.'));return;
      }
      length+=insert.length-(to-from);
    }
    this.doc.transact(()=>{for(const {from,to,insert} of changes){if(to>from)this.text.delete(from,to-from);if(insert)this.text.insert(from,insert)}},local);
  }
  finishComposition(){
    // Still composing: the composition's last text joins what it composed.
    this.takeInput?.();
    if(this.closed)return;
    this.composing=false;
    const changes=this.composed;this.composed=[];
    this.local(changes);
    const deferred=this.deferred;this.deferred=[];
    // An answer of a new generation ends the session, reporting why.
    try{for(const value of deferred)if(!this.closed)this.receive(value)}catch{}
    this.save();
  }
  // Sends once the change has painted, unless a send is in flight, which
  // sends what follows when it ends, or failed, after which it is retried
  // (retry). Says whether a send is coming.
  schedule(){
    if(this.closed || this.sending || this.sendFailed)return false;
    if(!this.sendQueued){this.sendQueued=true;afterPaint(()=>this.send())}
    return true;
  }
  // Sends the change schedule() queued, now.
  send(){if(this.sendQueued){this.sendQueued=false;if(!this.closed)this.flush().catch(()=>{})}}
  // Sends again what failed to send: on its backoff, or at once when the
  // server can be reached again.
  retry(){
    clearTimeout(this.retryTimer);this.retryTimer=0;
    if(this.closed || !this.sendFailed)return;
    this.sendFailed=false;
    if(this.pending || this.unsent)this.flush().catch(()=>{});
  }
  // Reports the state after an answer; a send that failed is still
  // reported until one succeeds.
  report(){
    if(this.sendFailed && this.sendError)this.status(null,true,this.sendError);
    else this.status(this.snapshot,this.unsent || !!this.pending);
  }
  retainPrivate(error){error.privateDraft=true;this.pending=null;this.unsent=false;this.stop();this.status(null,true,error)}
  receive(snapshot){
    if(this.closed)return;
    if(snapshot.unchanged){this.report();return}
    if(this.composing){this.deferred.push(snapshot);return}
    if(snapshot===this.initial){this.initial=null;this.settle();return}
    if(snapshot.liveRevision.split(':')[0]!==this.generation){
      const error=new Error('This note was saved, moved, restored, or compacted. Your draft is retained; reopen and reconcile before sharing it.');
      this.stop();this.status(null,true,error);throw error;
    }
    const update=decode(snapshot.update);
    // What this changes in the text reaches the editor through the observer.
    // The input can retire the session (a change that no longer matches).
    this.takeInput?.();
    if(this.closed)return;
    Y.applyUpdate(this.doc,update,remote);
    for(const [client,clock] of clocks(update))if(clock>(this.acknowledged.get(client)??0))this.acknowledged.set(client,clock);
    // An answer to an older request (a poll that crossed an acknowledgement)
    // brings its changes but not its older revision.
    if(sequence(snapshot.liveRevision)>=sequence(this.snapshot.liveRevision)){
      const text=this.text,{path,revision,version,liveRevision,pending}=snapshot;
      this.snapshot=snapshot.incremental?{path,revision,version,liveRevision,pending,get content(){return text.toString()}}:snapshot;
    }
    this.settle();
  }
  // Records and reports the state after an answer.
  settle(){
    if(this.unsent || this.pending)this.save();else this.store();
    this.report();
  }
  // This user's undo (or redo). Never inside an open IME composition: its
  // text is in the editor but not yet in the note, so the step's offsets
  // would change other characters in the editor than in the note.
  undo(redo=false){
    if(this.closed || this.composing || this.composed.length)return;
    this.takeInput?.();
    if(this.closed)return;
    redo?this.undoManager.redo():this.undoManager.undo();
  }
  // A failed send is not the poll's to retry, so reading goes on meanwhile.
  async poll(){
    if(this.closed || this.polling)return;
    this.polling=true;
    const failed=this.pollFailures;
    try{
      if((this.pending || this.unsent) && !this.sendFailed)await this.flush();
      const v=this.snapshot;
      this.receive(await this.request('read',v.liveRevision+'/'+v.version+'/'+v.pending));this.pollFailures=0;
      if(failed)this.retry();
    }catch(error){this.pollFailures=(this.pollFailures??0)+1;if(!this.closed)this.status(null,true,error)}finally{this.polling=false}
  }
  async flush(){
    if(this.closed)throw new Error('Live session ended; reopen and reconcile the retained draft.');
    if(this.sending){await this.sending;if(this.unsent || this.pending)return this.flush();return}
    if(!this.pending && !this.unsent)return;
    if(!this.pending){
      // Only what the server does not hold yet, unless it asked for everything.
      const update=this.complete?Y.encodeStateAsUpdate(this.doc):Y.encodeStateAsUpdate(this.doc,Y.encodeStateVector(this.acknowledged));
      this.pending={path:this.snapshot.path,generation:this.generation,operationId:crypto.randomUUID(),update:encode(update),since:this.snapshot.liveRevision};
      this.sentComplete=!!this.complete;this.unsent=false;this.store();
    }
    this.sending=(async()=>{
      const sent=this.pending;
      try{
        const snapshot=await this.request('update',sent);
        if(this.closed)return;
        this.pending=null;this.complete=false;this.sendFailed=false;this.sendError=null;this.sendFailures=0;
        clearTimeout(this.retryTimer);this.retryTimer=0;
        // The server holds the update it acknowledged.
        for(const [client,clock] of clocks(decode(sent.update)))if(clock>(this.acknowledged.get(client)??0))this.acknowledged.set(client,clock);
        this.receive(snapshot);
      }catch(error){
        if(this.closed)throw error;
        if(missingDependencies(error) && !this.sentComplete){
          // The refused update changed nothing: send the complete state once.
          this.pending=null;this.complete=true;this.unsent=true;this.store();return;
        }
        this.sendFailed=true;this.sendError=error;this.sendFailures=(this.sendFailures??0)+1;
        if(error.status===422 && error.code==='live_operation_rejected')this.retainPrivate(error);
        else {
          this.store();this.status(null,true,error);
          clearTimeout(this.retryTimer);this.retryTimer=setTimeout(()=>this.retry(),backoff(this.sendFailures));
        }
        throw error;
      }
    })();
    try{await this.sending}finally{this.sending=null}
    if(this.unsent && !this.closed)return this.flush();
  }
  // The answer is only what changed after the revision this tab holds.
  async checkpoint(){await this.flush();const snapshot=await this.request('checkpoint',{path:this.snapshot.path,since:this.snapshot.liveRevision});this.receive(snapshot);return snapshot}
  stop(){
    this.closed=true;this.loop?.stop();this.abort?.();clearTimeout(this.retryTimer);
    globalThis.removeEventListener?.('pagehide',this.leave);
    globalThis.removeEventListener?.('online',this.online);
    globalThis.document?.removeEventListener?.('visibilitychange',this.hidden);
  }
}
