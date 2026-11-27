// Application selection has one owner. Feature requests and editable drafts
// retain their own lifetimes; changing a route does not discard those drafts.
export function createApplicationState(initial) {
  let snapshot=Object.freeze({...initial});
  const listeners=new Set();
  return {
    read:()=>snapshot,
    notebook:()=>snapshot.notebook,
    account:()=>snapshot.account,
    note:()=>snapshot.note,
    update(change) {
      const next={...snapshot,...change};
      if(Object.keys(next).every(key=>Object.is(next[key],snapshot[key])))return snapshot;
      const previous=snapshot;snapshot=Object.freeze(next);
      for(const listener of listeners)listener(snapshot,previous);
      return snapshot;
    },
    subscribe(listener) {listeners.add(listener);return ()=>listeners.delete(listener);},
  };
}

// Whether the instance runs a feature module. /api/me names the modules it
// turns off, so an account without that list runs every module.
export function moduleOn(account,name) {return !(account?.modulesOff || []).includes(name);}

// Each sequence is owned by one independent request lifetime. Navigation,
// previews and metadata must not accidentally cancel one another. A fetch that
// passes `signal()` is aborted when a newer request of its sequence begins.
export function requestSequence() {
  let epoch=0,controller=null;
  function advance() {controller?.abort();controller=null;return ++epoch;}
  return {
    next:advance,current:()=>epoch,owns:token=>token===epoch,
    signal:()=>(controller??=new AbortController()).signal,
    begin(active=()=>true) {const token=advance();return ()=>token===epoch&&active();},
  };
}

// One owner's repeating request. A tick runs only while `active()` still holds
// and after the previous tick settles, so slow requests never overlap. Hidden
// tabs pause until visible again (unless `whileHidden`), and becoming visible
// runs a tick at once, as `wake()` does at any time (ending a failure's
// backoff early). A tick returning false ends the loop; `done` settles
// when it ends and rejects with an error the tick did not handle.
export function poll(tick,{every,first=every,whileHidden=false,active=()=>true}={}) {
  const page=globalThis.document;
  let timer,running=false,stopped=false,finish,fail;
  const done=new Promise((resolve,reject)=>{finish=resolve;fail=reject;});
  done.catch(()=>{});
  function stop(){
    if(stopped)return;
    stopped=true;clearTimeout(timer);page?.removeEventListener?.('visibilitychange',resume);finish();
  }
  function schedule(delay){clearTimeout(timer);if(!stopped)timer=setTimeout(run,typeof delay==='function'?delay():delay);}
  async function run() {
    if(stopped || running)return;
    if(!active()){stop();return;}
    if(page?.hidden && !whileHidden)return;
    running=true;let more;
    try{more=await tick();}
    catch(error){fail(error);stop();return;}
    finally{running=false;}
    if(more===false || !active())stop();else schedule(every);
  }
  function wake(){if(!running && !stopped){clearTimeout(timer);run();}}
  function resume(){if(!page.hidden)wake();}
  page?.addEventListener?.('visibilitychange',resume);
  schedule(first);
  return {stop,done,wake};
}
