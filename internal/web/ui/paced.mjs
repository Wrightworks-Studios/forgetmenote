// Runs work that follows a stream of changes, such as typing, once the
// changes pause, rather than at each one: soon() asks for it pause ms after
// the last request, and at most most ms after the first one still waiting,
// so a stream that does not pause still sees it at least that often. now()
// runs it at once, superseding a run that is waiting.
export function paced(run,{pause=250,most=1000,clock=()=>Date.now(),timers=globalThis}={}) {
 let timer=0,due=0;
 const now=()=>{timers.clearTimeout(timer);timer=0;due=0;run()};
 return {
  soon(){
   timers.clearTimeout(timer);
   due||=clock()+most;
   timer=timers.setTimeout(now,Math.max(0,Math.min(pause,due-clock())));
  },
  now,
 };
}
