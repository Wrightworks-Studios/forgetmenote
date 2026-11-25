import {test} from 'node:test';
import assert from 'node:assert/strict';
import {paced} from './paced.mjs';

// A clock and timers that advance only when told to.
function fakeTime() {
 let time=0,next=1;const waiting=new Map();
 return {
  clock:()=>time,
  timers:{setTimeout(fn,ms){waiting.set(next,{fn,at:time+ms});return next++},clearTimeout(id){waiting.delete(id)}},
  advance(ms){
   const end=time+ms;
   for(;;){
    const [id,due]=[...waiting].sort((a,b)=>a[1].at-b[1].at)[0] || [];
    if(!due || due.at>end)break;
    waiting.delete(id);time=due.at;due.fn();
   }
   time=end;
  },
 };
}

test('paced work waits for the changes to pause, not each change',()=>{
 const time=fakeTime();let runs=0;
 const badge=paced(()=>runs++,time);
 for(let key=0;key<5;key++){badge.soon();time.advance(50)}
 assert.equal(runs,0,'it ran during typing');
 time.advance(199);assert.equal(runs,0,'it ran before the pause');
 time.advance(1);assert.equal(runs,1,'it did not run 250 ms after the last change');
 time.advance(5000);assert.equal(runs,1);
});

test('paced work runs at least once a second while the changes do not pause',()=>{
 const time=fakeTime(),at=[];
 const badge=paced(()=>at.push(time.clock()),time);
 // A key every 50 ms for 3 s.
 for(let key=0;key<60;key++){badge.soon();time.advance(50)}
 assert.deepEqual(at,[1000,2000,3000]);
 time.advance(5000);assert.deepEqual(at,[1000,2000,3000]);
});

test('paced work runs at once when asked, superseding the wait',()=>{
 const time=fakeTime();let runs=0;
 const badge=paced(()=>runs++,time);
 badge.soon();badge.now();assert.equal(runs,1);
 time.advance(2000);assert.equal(runs,1,'the superseded wait ran too');
});
