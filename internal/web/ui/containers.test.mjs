import test from 'node:test';
import assert from 'node:assert/strict';
import {pendingContainer} from './containers.mjs';
test('pending collection scalars match supported server quoting and escapes',()=>{
 for(const value of ['draft','"draft"',"'draft'",'draft # comment','"draft"#comment'])assert.equal(pendingContainer(`---\nfmn-container: ${value}\n---\n`),'draft',value);
 for(const value of ['archive','"\\u0061rchive"','"\\U00000061rchive"','"\\x61rchive"','"\\141rchive"',"'archive' # comment"])assert.equal(pendingContainer(`---\nfmn-container: ${value}\n---\n`),'archive',value);
 for(const value of ['"archive',"archive'",'"archive\'','"archive" garbage','"\\qarchive"','"\\777rchive"','archive#comment',"'arch''ive'",'archive\n\n  nested: value','draft\n# comment\n  nested: value','draft\nfmn-container: archive'])assert.equal(pendingContainer(`---\nfmn-container: ${value}\n---\n`),'invalid',value);
 assert.equal(pendingContainer('---\nstatus: archived\n---\nfmn-container: archive'),'');
 assert.equal(pendingContainer('---\nfmn-container: draft'),'invalid');
 assert.equal(pendingContainer('---\n'+'# padding\n'.repeat(6600)+'fmn-container: draft\n---\n'),'invalid');
});

test('quoted reserved keys are repairable invalid metadata, including escaped duplicates',()=>{
 for(const key of ['"fmn-container"',"'fmn-container'",'"fmn\\u002dcontainer"','"fmn\\x2dcontainer"']) {
  for(const value of ['archive','draft','future'])assert.equal(pendingContainer(`---\n${key}: ${value}\n---\n`),'invalid');
  assert.equal(pendingContainer(`---\nfmn-container: archive\n${key}: draft\n---\n`),'invalid');
 }
});

test('the pending collection costs what the front matter costs, not the note',()=>{
 const body='Call me Ishmael. Some years ago.\n'.repeat(1<<19),note='---\ntitle: x\nfmn-container: draft\n---\n'+body;
 assert.equal(pendingContainer(note),'draft');
 assert.equal(pendingContainer('---\ntitle: x\n'+body),'invalid');
 assert.equal(pendingContainer('---\r\nfmn-container: archive\r\n---\r\n'+body),'archive');
 const start=performance.now();
 for(let i=0;i<20;i++)pendingContainer(note);
 assert((performance.now()-start)/20<5,'scanned past the front matter');
});

test('reading the front matter line by line keeps the whole-note result',()=>{
 // The previous implementation, which split the whole note into lines.
 const whole=content=>{
  if(!/^---\r?\n/.test(content))return null;
  const lines=content.split(/(?<=\n)/),encoder=new TextEncoder();
  let offset=encoder.encode(lines[0]).length,end=-1;
  for(let i=1;i<lines.length;i++){if(lines[i].replace(/[\r\n]+$/,'')==='---'){end=i;break}offset+=encoder.encode(lines[i]).length;if(offset>65536)break}
  return end<0?'invalid':lines.slice(1,end).join('');
 };
 let seed=99;const random=n=>{seed=(Math.imul(seed,1664525)+1013904223)>>>0;return seed%n};
 const pieces=['---\n','---\r\n','fmn-container: draft\n','fmn-container: archive\r\n','  nested: x\n','# c\n','\n','title: é\n','---','x'];
 for(let i=0;i<3000;i++){
  const text='---\n'+Array.from({length:random(8)},()=>pieces[random(pieces.length)]).join('');
  const expected=whole(text),actual=pendingContainer(text);
  if(expected==='invalid')assert.equal(actual,'invalid',JSON.stringify(text));
  else assert.equal(actual,pendingContainer('---\n'+expected+'---\n'),JSON.stringify(text));
 }
});
