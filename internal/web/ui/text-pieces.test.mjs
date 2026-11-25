import test from 'node:test';
import assert from 'node:assert/strict';
import {TextPieces} from './text-pieces.mjs';
import {applyChanges} from './markdown-editor.mjs';

test('changes apply as they do to a string, across pieces, and invert exactly',()=>{
 let seed=7;const random=n=>{seed=(seed*1103515245+12345)%2147483648;return seed%n};
 const start='# Title 😀\n\n'+'Call me Ishmael. '.repeat(3000),pieces=new TextPieces(start);
 let text=start;
 for(let i=0;i<500;i++){
  const changes=[];let length=text.length;
  for(let k=0;k<1+random(3);k++){
   const from=random(length+1),to=Math.min(length,from+(random(4)?random(3):random(40000)));
   const insert=random(3)?'x'.repeat(random(3)):'Ishmael 🐋 '.repeat(random(3000));
   changes.push({from,to,insert});length+=insert.length-(to-from);
  }
  const expected=applyChanges(text,changes);
  assert.deepEqual(pieces.apply(changes),expected.inverse);
  text=expected.text;
  assert.equal(pieces.length,text.length);
  if(i%50===0)assert.equal(pieces.text,text);
 }
 assert.equal(pieces.text,text);
 pieces.apply([{from:0,to:text.length,insert:''}]);assert.equal(pieces.text,'');
 pieces.apply([{from:0,to:0,insert:'again'}]);assert.equal(pieces.text,'again');
});
test('changes that do not fit change nothing',()=>{
 const pieces=new TextPieces('abc');
 for(const changes of [[{from:1,to:4,insert:''}],[{from:0,to:0,insert:'x'},{from:2,to:1,insert:''}],[{from:0.5,to:1,insert:''}],[{from:0,to:1}]])
  assert.throws(()=>pieces.apply(changes),RangeError);
 assert.equal(pieces.text,'abc');assert.equal(pieces.length,3);
});
// Counts the characters string slices copy while fn runs: the work changes
// do, whatever the host's speed.
function copiedWhile(fn){
 const slice=String.prototype.slice;let copied=0;
 String.prototype.slice=function(...args){const part=slice.apply(this,args);copied+=part.length;return part};
 try{fn()}finally{String.prototype.slice=slice}
 return copied;
}
test('a keystroke in a long text copies a piece, not the text',()=>{
 // Slicing the whole 1.2 MB text instead copies it at every keystroke:
 // 2,000 keystrokes took 1.2 s that way on the development host.
 const pieces=new TextPieces('Call me Ishmael. Some years ago — never mind how long precisely. '.repeat(19000));
 let at=pieces.length>>1;
 const copied=copiedWhile(()=>{
  for(let i=0;i<2000;i++){
   if(i%20===19)pieces.apply([{from:at-1,to:at,insert:''}]),at--;
   else pieces.apply([{from:at,to:at,insert:'a'}]),at++;
   // The editor and the app read its length on every keystroke.
   assert(pieces.text.length>0);
  }
 });
 // A piece is at most two of 8,192 characters, and a keystroke copies the
 // one it touches; slicing the text would copy 1.2 million at each.
 assert(copied<2000*2*16384,'2,000 keystrokes copied '+copied+' characters');
 assert(copiedWhile(()=>pieces.apply([{from:0,to:pieces.length,insert:''}]))<=1216000+2*16384);
});
