import {test} from 'node:test';
import assert from 'node:assert/strict';
import {applyChatEvent,createTurnSubmission,chatCitation,attachmentSnapshot} from './chat.mjs';

test('live data citations stay inside their private transcript',()=>{
 const citation={kind:'data',project:'here',url:'#data-receipt-'+ 'a'.repeat(64),text:'{"count":7}'};
 assert.equal(chatCitation(citation,'here').kind,'data');
 assert.equal(chatCitation(citation,'other'),null);
 assert.equal(chatCitation({...citation,url:'https://evil.test'},'here'),null);
});

test('durable stream replay adds text once, detects gaps, and keeps complete paged turns',()=>{
 const state={eventCursor:7,turns:[{id:'old',role:'user',content:'Earlier page'}],runs:[]};
 const event={sequence:8,run:'run1',type:'text_delta',data:{turnId:'answer',text:'Hello'}};
 const next=applyChatEvent(state,event);
 assert.equal(next.turns[1]?.content,'Hello');
 assert.equal(next.turns[0].content,'Earlier page');
 assert.equal(next.eventCursor,8);
 assert.equal(applyChatEvent(next,event),next);
 assert.equal(applyChatEvent(next,{...event,sequence:10}).gap,true);
 assert.equal(next.turns[1].content,'Hello');
});
test('final answers replace streamed text, while prepared approvals retain exact server payload',()=>{
 let state={eventCursor:0,turns:[],runs:[]};
 for(const e of [
  {type:'text_delta',data:{turnId:'answer',text:'Partial'}},
  {type:'assistant',data:{id:'answer',role:'assistant',content:'Complete',citations:[{path:'a.md',revision:'a'.repeat(40)}]}},
  {type:'approval_required',data:{id:'action1',name:'empty_trash',arguments:{revision:'r',confirmation:'manifest'},confirmation:'opaque',result:{count:3}}},
  {type:'state',data:{state:'awaiting_confirmation'}}
 ])state=applyChatEvent(state,{sequence:state.eventCursor+1,run:'run1',...e});
 assert.equal(state.turns.length,1);assert.equal(state.turns[0].content,'Complete');
 assert.deepEqual(state.runs[0].operations[0].arguments,{revision:'r',confirmation:'manifest'});
 assert.equal(state.runs[0].operations[0].confirmation,'opaque');
 assert.equal(state.runs[0].state,'awaiting_confirmation');
});
test('retry after a lost acknowledgement sends the original immutable submission ID and payload',()=>{
 let ids=0;const send=createTurnSubmission(()=>`submission-${++ids}`),options={mode:'ask'},attachments=[];
 const first=send.prepare('question',options,attachments);options.mode='edit';attachments.push({kind:'private',content:'secret'});
 const retry=send.prepare('question',{mode:'ask'},[]);
 assert.deepEqual(retry,first);assert.equal(ids,1);assert.equal(retry.options.mode,'ask');assert.deepEqual(retry.attachments,[]);
 assert.throws(()=>send.prepare('changed',{mode:'ask'},[]),/retry/i);
 send.confirmed();assert.notEqual(send.prepare('next',{mode:'ask'},[]).submissionId,first.submissionId);
});
test('committed and live attachments never copy editor content; private snapshots are deliberate',()=>{
 let reads=0;const note={path:'a.md',revision:'git',liveRevision:'live',content:'Never infer this'};
 const draft=()=>{reads++;return {path:'a.md',revision:'git',content:'Explicit private snapshot'}};
 assert.deepEqual(attachmentSnapshot('committed',note,draft),{kind:'committed',path:'a.md',revision:'git'});
 assert.deepEqual(attachmentSnapshot('live',note,draft),{kind:'live',path:'a.md',revision:'git',liveRevision:'live'});
 assert.equal(reads,0);
 assert.deepEqual(attachmentSnapshot('private',note,draft),{kind:'private',path:'a.md',revision:'git',content:'Explicit private snapshot'});
 assert.equal(reads,1);
});
test('citations only open pinned same-notebook notes or explicit public sources',()=>{
 const revision='a'.repeat(40);
 assert.equal(chatCitation({project:'other',path:'secret.md',revision},'here'),null);
 for(const path of ['../secret.md','https://evil.test/a.md','javascript:alert(1)','/a.md'])assert.equal(chatCitation({path,revision},'here'),null);
 assert.equal(chatCitation({path:'a.md'},'here'),null);
 assert.deepEqual(chatCitation({path:'a.md',revision,heading:'Heading'},'here'),{kind:'note',path:'a.md',revision,heading:'Heading'});
 assert.equal(chatCitation({url:'javascript:alert(1)'},'here'),null);
 assert.deepEqual(chatCitation({url:'https://example.com/page'},'here'),{kind:'source',url:'https://example.com/page'});
});

test('durable notebook review events show actual coverage and continuation cursor',()=>{
 const coverage={notes:[{path:'a.md',revision:'a'.repeat(40),hash:'hash',state:'pending',cursor:2048,total:4096,summary:'First section'}],complete:false,batchPath:'a.md'};
 const progress={examined:0,total:1,passagesExamined:2,passagesTotal:4,complete:false,message:'Examined 0 of 1 notes (2 of 4 original passages)'};
 const state=applyChatEvent({eventCursor:0,turns:[],runs:[{id:'review-run',review:coverage}]},{sequence:1,run:'review-run',type:'review',data:progress});
 assert.deepEqual(state.runs[0].review,coverage);assert.deepEqual(state.runs[0].reviewProgress,progress);assert.equal(state.runs[0].progress,progress.message);
});

test('explicit notebook and repository citations open pinned local documents',()=>{
 const revision='a'.repeat(40),source='b'.repeat(32);
 const note=chatCitation({kind:'notebook',project:'other',path:'guide.md',revision,url:'/?project=other&note=guide.md&revision='+revision+'#setup'},'here');
 assert.equal(note?.kind,'document');
 assert.deepEqual(note.ref,{project:'other',origin:'notes',path:'guide.md',revision,heading:'setup'});
 const code=chatCitation({kind:'repository',project:'code',source,path:'server.py',revision,startLine:31,endLine:40,url:'https://github.com/example/code/blob/'+revision+'/server.py'},'here');
 assert.equal(code?.kind,'document');
 assert.deepEqual(code.ref,{project:'code',origin:'repository',source,path:'server.py',revision,startLine:31,endLine:40});
 assert.equal(new URL(code.url,'https://notes.example').searchParams.get('revision'),revision);
 for(const path of ['../private','/private','https://evil.test/file'])assert.equal(chatCitation({kind:'notebook',project:'other',path,revision},'here'),null);
 assert.equal(chatCitation({kind:'notebook',project:'../other',path:'guide.md',revision},'here'),null);
});
