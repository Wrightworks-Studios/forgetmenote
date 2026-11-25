// Writing analysis cost and results on a synthetic chapter book and, given
// paths, on real notes (node scripts/writing_benchmark.mjs [note.md ...]).
// The digests pin the results: a parser change must leave them unchanged.
import {createHash} from 'node:crypto';
import {readFileSync} from 'node:fs';
import {analyzeWriting,projectWriting} from '../frontend/writing-model.mjs';

const digest=value=>createHash('sha256').update(JSON.stringify(value)).digest('hex').slice(0,16);
const median=values=>values.sort((a,b)=>a-b)[values.length>>1];
function measure(name,source,runs=5){
 const parse=[],checks=[];let result,projection;
 for(let i=0;i<runs;i++){
  const start=performance.now();projection=projectWriting(source);
  const parsed=performance.now();result=analyzeWriting(source,{terms:[{name:'Elizabeth'},{name:'Ishmael'}]},projection);
  const end=performance.now();parse.push(parsed-start);checks.push(end-parsed);
 }
 return {name,bytes:Buffer.byteLength(source),words:result.stats.words,sections:result.sections.length,findings:result.findings.length,mentions:result.matches.length,
  parseMs:Math.round(median(parse)),checksMs:Math.round(median(checks)),projection:digest(projection),result:digest(result)};
}
const synthetic=('# Chapter\n\n'+('Elizabeth sees the quiet harbour and walks toward home. '.repeat(11))+'\n\n').repeat(1000);
const rows=[measure('synthetic',synthetic),...process.argv.slice(2).map(path=>measure(path,readFileSync(path,'utf8'),3))];
console.log(JSON.stringify(rows,null,2));
