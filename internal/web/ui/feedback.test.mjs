import {test} from 'node:test';
import assert from 'node:assert/strict';
import {feedback} from './feedback.mjs';

test('feedback preserves persistent errors when an earlier success expires',async()=>{
 const target={dataset:{},classList:{toggle(){}},setAttribute(name,value){this[name]=value}};
 feedback(target,'Saved',{level:'success',duration:10});
 assert.equal(target.role,'status');assert.equal(target.hidden,false);
 feedback(target,'Save failed');
 await new Promise(resolve=>setTimeout(resolve,25));
 assert.equal(target.role,'alert');assert.equal(target.hidden,false);assert.equal(target.textContent,'Save failed');
 feedback(target,'');assert.equal(target.hidden,true);
});
