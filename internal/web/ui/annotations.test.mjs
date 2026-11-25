import {test} from 'node:test';
import assert from 'node:assert/strict';
import {findAnchor,anchorAt,projectSegments} from './annotations.mjs';

test('anchors use quote and context and survive text inserted elsewhere',()=>{
 const text='Heading A bright star shines.';
 const a=anchorAt(text,10,21);
 assert.equal(a.quote,'bright star');
 assert.deepEqual(findAnchor('New introduction. '+text,a),{from:28,to:39});
 assert.equal(findAnchor('Heading A dull moon shines.',a),null);
});
test('ambiguous or moved-without-context passages remain unattached',()=>{
 assert.equal(findAnchor('same words and same words',{quote:'same words',prefix:'',suffix:''}),null);
 assert.deepEqual(findAnchor('First same words. Second same words.',{quote:'same words',prefix:'Second ',suffix:'.'}),{from:25,to:35});
 assert.equal(findAnchor('Elsewhere same words changed.',{quote:'same words',prefix:'Original ',suffix:' remain.'}),null);
});
test('projection collapses whitespace while retaining text positions',()=>{
 const p=projectSegments([{text:' A\n ',points:[0,1,2,3]},{text:'bold',points:[10,11,12,13]},{text:'\tword ',points:[20,21,22,23,24,25]}]);
 assert.equal(p.text,'A bold word');
 assert.equal(p.points[2],10);
 assert.equal(p.points.at(-1),24);
});
test('context holds 64 code points on each side, astral ones included',()=>{
 const before='\u{1F600}'.repeat(70)+'a',after='b'+'\u{1F600}'.repeat(70),text=before+' quote '+after;
 const a=anchorAt(text,before.length,text.length-after.length);
 assert.deepEqual(a,{quote:'quote',prefix:'\u{1F600}'.repeat(62)+'a ',suffix:' b'+'\u{1F600}'.repeat(62)});
 assert.deepEqual(findAnchor(text,a),{from:before.length+1,to:before.length+6});
 assert.equal(findAnchor(text,{...a,prefix:'x'+a.prefix,suffix:a.suffix+'x'}),null);
});
