import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {analyzeWriting,parseTerms} from '../../../frontend/writing-model.mjs';

for(const fixture of JSON.parse(readFileSync(new URL('../../knowledge/testdata/writing.json',import.meta.url))))test('shared prose fixture: '+fixture.name,()=>{
 assert.equal(analyzeWriting(fixture.markdown).stats.words,fixture.words);
});

test('writing counts visible prose and preserves exact source ranges',()=>{
 const source='---\ntitle: Hidden words\n---\n# Chapter one\n\nRead [the guide](https://example.com) and **enjoy** it.\n\n```\nHidden code\n```\n<!-- hidden -->\n';
 const result=analyzeWriting(source);
 assert.equal(result.stats.words,8);assert.equal(result.stats.paragraphs,1);
 assert.equal(result.sections[0].words,8);assert.equal(result.sections[0].from,source.indexOf('#'));
});
test('checks are local, bounded, literal and skip metadata, links and code',()=>{
 const source='A **very very** clear  sentence , today. `code code`\n\nCaf&eacute; and café.\n';
 const result=analyzeWriting(source,{terms:[{name:'café'}]});
 assert.deepEqual(result.findings.filter(f=>f.replacement!==undefined).map(f=>[f.rule,source.slice(f.from,f.to),f.replacement]),[['spacing','  ',' '],['punctuation',' ,',','],['duplicate',' very','']]);
 assert.equal(result.matches.length,2);assert.equal(source.slice(result.matches[0].from,result.matches[0].to),'Caf&eacute;');
});
test('outline owns words once and rolls children and scenes up',()=>{
 const r=analyzeWriting('# Act one\n\nHello.\n\n## Chapter\n\nTwo words.\n\n***\n\nLast scene.\n');
 assert.deepEqual(r.sections.map(s=>[s.title,s.words,s.subtreeWords]),[['Act one',3,8],['Chapter',3,5],['Scene break',2,2]]);
});
test('terms are literals with Unicode boundaries, aliases and pagination',()=>{
 const source='Lizzy saw Elizabeth and Elizabethan. Café caféine. C++.';
 const terms=parseTerms('Elizabeth | Lizzy\nCafé\nC++');
 const first=analyzeWriting(source,{terms,limit:2});assert.equal(first.next,2);assert.deepEqual(first.matches.map(m=>m.text),['Lizzy','Elizabeth']);
 const second=analyzeWriting(source,{terms,limit:2,after:first.next});assert.deepEqual(second.matches.map(m=>m.text),['Café','C++']);assert.equal(second.next,0);
 assert.throws(()=>parseTerms('x'.repeat(101)));
});

test('a valid shorter alias still matches when the longer name is only a prefix',()=>{
 const result=analyzeWriting('Elizabeth Bennetton met Elizabeth Bennet.',{terms:[{name:'Elizabeth Bennet',aliases:['Elizabeth']}]});
 assert.deepEqual(result.matches.map(m=>m.text),['Elizabeth','Elizabeth Bennet']);
});
