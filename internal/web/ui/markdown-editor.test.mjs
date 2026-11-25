import test from 'node:test';
import assert from 'node:assert/strict';
import {MarkdownEditor,TextHistory,applyChanges,interval,rememberedFormat,shownFormat} from './markdown-editor.mjs';

const element=()=>({hidden:false,textContent:'',attributes:{},dataset:{},setAttribute(name,value){this.attributes[name]=value},replaceChildren(){}});
const memory=(values={})=>({values,getItem(key){return key in this.values?this.values[key]:null},setItem(key,value){this.values[key]=String(value)}});
// The editor's one view as the editor sees it: it keeps its own copy of the
// text and reports edits as changes, as CodeMirror does; a mode switch only
// reconfigures it.
function views(){
  const created=[],panes=[];
  const view=({text,changed,format,privateDraft,videoHost})=>{
    const self={text,changed,format,privateDraft,videoHost,privateDrafts:[],formats:[format],applied:[],destroyed:false,revealed:[],
      apply(changes){if(self.refuse)throw Error('Out of step');self.applied.push(changes);self.text=applyChanges(self.text,changes).text},
      type(changes){self.text=applyChanges(self.text,changes).text;self.changed(changes)},
      setFormat(next){if(next!==self.format){self.format=next;self.formats.push(next)}},
      readonly(value){self.readOnly=value},focus(){},refresh(){},select(from,to){self.selected=[from,to]},selection:()=>({start:0,end:0,direction:'none'}),
      replace(insert,from,to){self.type([{from,to,insert}])},
      setComments(comments){self.comments=comments},commentAnchor:()=>({quote:'selected'}),
      focusComment(id,anchor){self.focused=[id,anchor];return anchor?.quote==='placed'},
      reveal(at){self.revealed.push(at)},setPrivateDraft(value){self.privateDrafts.push(value)},destroy(){self.destroyed=true},view:{}};
    created.push(self);return self;
  };
  return {created,panes,load:{
    editor:async()=>({createSource:view}),
    preview:async()=>({mountPreview:options=>{const pane={...options,destroyed:false,destroy(){pane.destroyed=true}};panes.push(pane);return pane}}),
  }};
}
function setup({live=null,storage=memory(),wide=()=>true,privateDraft}={}){
  const {created,panes,load}=views(),host={dataset:{}},pane=element();
  const buttons={visualButton:element(),sourceButton:element(),splitButton:element()};
  const editor=new MarkdownEditor({host,root:element(),pane,toolbar:element(),...buttons,hint:element(),liveHistory:live,load,storage,wide,privateDraft});
  const inputs=[];editor.addEventListener('input',event=>inputs.push(event.changes));
  return {editor,created,panes,inputs,host,pane,buttons,storage,shown:()=>created.at(-1)};
}
// Lets the view load and mount.
const settle=async editor=>{for(let i=0;i<4;i++){await editor.rendered;await new Promise(resolve=>setTimeout(resolve))}};

test('text changes apply in order and invert exactly',()=>{
 const text='Alpha 😀\n\nBeta',changes=[{from:0,to:5,insert:'ALPHA!'},{from:7,to:7,insert:' plus'},{from:0,to:0,insert:'# '}];
 const {text:after,inverse}=applyChanges(text,changes);
 assert.equal(after,'# ALPHA!  plus😀\n\nBeta');assert.equal(applyChanges(after,inverse).text,text);
 assert.throws(()=>applyChanges('ab',[{from:1,to:3,insert:''}]),RangeError);
 for(const [a,b] of [['😀 a','😁 a'],['x😀','x😁'],['same','same'],['','new'],['old','']]){
  const change=interval(a,b);assert.equal(applyChanges(a,[change]).text,b);
  assert(!/[\uD800-\uDBFF]$/.test(a.slice(0,change.from)));assert(!/^[\uDC00-\uDFFF]/.test(a.slice(change.to)));
 }
});
test('history joins quick edits, splits at boundaries and undoes exactly',()=>{
 let time=0;const history=new TextHistory({delay:500,now:()=>time});
 let text='abc';
 const edit=changes=>{const result=applyChanges(text,changes);text=result.text;history.record(changes,result.inverse)};
 edit([{from:3,to:3,insert:'d'}]);time=100;edit([{from:4,to:4,insert:'e'}]);history.boundary();edit([{from:0,to:1,insert:'A'}]);
 text=applyChanges(text,history.take(false)).text;assert.equal(text,'abcde');
 text=applyChanges(text,history.take(false)).text;assert.equal(text,'abc');
 assert.equal(history.take(false),null);
 text=applyChanges(text,history.take(true)).text;assert.equal(text,'abcde');
 time=2000;edit([{from:5,to:5,insert:'!'}]);assert.equal(history.can(true),false,'a new edit kept the redo step');
});
test('view edits become one input event with their changes, and the editor text follows them',async()=>{
 const {editor,shown,inputs,host}=setup();
 editor.value='# Title\n\nBody\n';editor.show(true);await settle(editor);
 assert.equal(shown().text,'# Title\n\nBody\n');assert.equal(shown().format,'visual');
 shown().type([{from:13,to:13,insert:' text'}]);
 assert.deepEqual(inputs,[[{from:13,to:13,insert:' text'}]]);
 assert.equal(editor.value,'# Title\n\nBody text\n');assert.equal(host.markdown,editor.value);
 assert.deepEqual(shown().applied,[],'the view received its own edit back');
});
test('remote changes reach the shown view as changes and are never reported as input',async()=>{
 const {editor,shown,inputs}=setup();
 editor.value='one\n\ntwo\n';editor.choose('source');editor.show(true);await settle(editor);
 editor.applyRemote([{from:5,to:8,insert:'TWO'}]);
 assert.deepEqual(shown().applied,[[{from:5,to:8,insert:'TWO'}]]);assert.equal(shown().text,'one\n\nTWO\n');
 assert.equal(editor.value,'one\n\nTWO\n');assert.deepEqual(inputs,[]);
 // A live session joining with other text moves the editor with one change.
 editor.sync('one\n\nTWO and more\n');assert.deepEqual(shown().applied.at(-1),[{from:8,to:8,insert:' and more'}]);
});
test('input the view has not read yet is reported as an edit on request, before remote changes',async()=>{
 const {editor,shown,inputs}=setup();
 editor.takeInput();
 editor.value='Local.\n';editor.show(true);await settle(editor);
 // The browser changed the text; the view's observer has not reported it.
 const view=shown();view.unread=[{from:6,to:6,insert:'日本語'}];
 view.takeInput=()=>{const changes=view.unread;view.unread=null;if(changes)view.type(changes)};
 editor.takeInput();
 assert.deepEqual(inputs,[[{from:6,to:6,insert:'日本語'}]]);assert.equal(editor.value,'Local.日本語\n');
 editor.applyRemote([{from:0,to:0,insert:'AI '}]);
 assert.equal(editor.value,'AI Local.日本語\n');assert.equal(view.text,editor.value);
 editor.takeInput();assert.equal(inputs.length,1,'nothing unread is reported twice');
});
// Anything that dispatches to the view, or acts on offsets of its text, reads
// the view's unread input first: a dispatch over unread input throws it away,
// and offsets taken before it are offsets of older text.
test('every call that reaches the view reads its unread input first',async()=>{
 const live={active:()=>false,run(){},enabled:()=>null,boundary(){}};
 const {editor,shown,inputs}=setup({live});
 editor.value='Local.\n';editor.show(true);await settle(editor);
 const view=shown();
 view.takeInput=()=>{const changes=view.unread;view.unread=null;if(changes)view.type(changes)};
 // The browser typed `insert` at the end of the text the view still shows.
 const typing=insert=>{const at=view.text.length-1;view.unread=[{from:at,to:at,insert}]};
 const calls={
  sync:()=>editor.sync({toString:()=>editor.value}),
  undo:()=>editor.undo(),
  selection:()=>editor.selection(),
  setRangeText:()=>editor.setRangeText('',0,0,'end'),
  // Offsets of the text before the input are refused.
  replaceRange:()=>assert.equal(editor.replaceRange(view.text,0,0,''),false),
  selectRange:()=>assert.equal(editor.selectRange(view.text,0,1),false),
  choose:()=>editor.choose(editor.format==='visual'?'source':'visual'),
  readOnly:()=>{editor.readOnly=false},
  setComments:()=>editor.setComments([]),
  commentAnchor:()=>editor.commentAnchor(),
  focusComment:()=>editor.focusComment('none'),
  focus:()=>editor.focus(),
  reveal:()=>editor.reveal([{from:0,to:0,insert:''}]),
  privateDraftChanged:()=>editor.privateDraftChanged(),
 };
 for(const [name,call] of Object.entries(calls)){
  const before=view.text,count=inputs.length;
  typing(name);call();
  assert.equal(view.unread,null,name+' left input unread');
  assert.deepEqual(inputs[count],[{from:before.length-1,to:before.length-1,insert:name}],name);
  if(name!=='undo')assert.equal(editor.value,view.text,name);
 }
});
test('a view created while changes arrive starts from the current text',async()=>{
 const {editor,shown}=setup();
 editor.value='base';editor.show(true);
 editor.applyRemote([{from:4,to:4,insert:' remote'}]);
 await settle(editor);
 assert.equal(shown().text,'base remote');assert.deepEqual(shown().applied,[]);
});
test('edits of the view of a note the editor no longer shows are ignored',async()=>{
 const {editor,created,inputs}=setup();
 editor.value='text';editor.show(true);await settle(editor);
 const old=created[0];editor.value='next note';
 old.type([{from:0,to:0,insert:'stale '}]);
 await settle(editor);
 assert.equal(editor.value,'next note');assert.deepEqual(inputs,[]);assert(old.destroyed);
 assert.equal(created.length,2);assert.equal(created[1].text,'next note');
});
test('without a live session the editor undoes this user’s edits; with one, the session does',async()=>{
 let liveActive=false;const runs=[];
 const live={active:()=>liveActive,run:redo=>runs.push(redo),enabled:redo=>liveActive?redo===false:null,boundary(){}};
 const {editor,shown,inputs}=setup({live});
 editor.value='abc';editor.choose('source');editor.show(true);await settle(editor);
 shown().type([{from:3,to:3,insert:'d'}]);
 assert(editor.canUndo(false));editor.undo();
 assert.equal(editor.value,'abc');assert.equal(shown().text,'abc');assert.deepEqual(inputs.at(-1),[{from:3,to:4,insert:''}]);
 editor.undo(true);assert.equal(editor.value,'abcd');
 // Changes made elsewhere make the local steps stale.
 editor.applyRemote([{from:0,to:0,insert:'>'}]);assert.equal(editor.canUndo(false),false);
 liveActive=true;shown().type([{from:5,to:5,insert:'e'}]);editor.undo();editor.undo(true);
 assert.deepEqual(runs,[false,true]);assert.equal(editor.value,'>abcde');assert.equal(editor.canUndo(false),true);assert.equal(editor.canUndo(true),false);
});
test('a writing-tool fix is one edit of exact text, shown in the view and reported once',async()=>{
 const {editor,shown,inputs}=setup();
 editor.value='A very very clear sentence.\n';editor.show(true);await settle(editor);
 assert.equal(editor.replaceRange('stale','0','1','x'),false);
 assert.equal(editor.replaceRange(editor.value,2,12,'very '),true);
 assert.equal(editor.value,'A very clear sentence.\n');assert.deepEqual(inputs,[[{from:2,to:12,insert:'very '}]]);
 assert.equal(shown().text,editor.value);
 editor.readOnly=true;assert.equal(editor.replaceRange(editor.value,0,1,'B'),false);assert.equal(shown().readOnly,true);
});
test('inserting text without a shown view edits the note text directly',()=>{
 const {editor,inputs}=setup();
 editor.value='before after';editor.setRangeText('middle ',7,7,'end');
 assert.equal(editor.value,'before middle after');assert.deepEqual(inputs,[[{from:7,to:7,insert:'middle '}]]);
 assert.equal(editor.selectionStart,14);
});
test('notes with references, footnotes or unclosed front matter open in Visual like any other',async()=>{
 for(const text of ['[a]: https://example.com\n','Text.[^1]\n\n[^1]: Note.\n','---\ntitle: open\n\nText\n']){
  const {editor,shown}=setup();
  editor.value=text;editor.show(true);await settle(editor);
  assert.equal(editor.format,'visual',text);assert.equal(shown().format,'visual');assert.equal(shown().text,text);
  assert.equal(editor.hint.hidden,true,text);
  // A remote change that adds a definition changes nothing else.
  editor.applyRemote([{from:text.length,to:text.length,insert:'\n[b]: https://example.org\n'}]);
  assert.equal(editor.format,'visual');assert.equal(shown().destroyed,false);
 }
});
test('switching modes reconfigures the one view, never remounts it or changes the text, and is remembered by the browser',async()=>{
 const storage=memory();
 const {editor,created,inputs,host,buttons,pane,panes}=setup({storage});
 editor.value='# Note\n\nText.\n';editor.show(true);await settle(editor);
 // A pane switched away from before it loaded never mounts.
 editor.choose('split');editor.choose('visual');
 for(const format of ['source','split','visual','split','source','visual']){editor.choose(format);await settle(editor);}
 assert.equal(created.length,1);assert.deepEqual(created[0].formats,['visual','split','visual','source','split','visual','split','source','visual']);
 assert.equal(editor.value,'# Note\n\nText.\n');assert.deepEqual(inputs,[]);assert.deepEqual(created[0].applied,[]);
 assert.equal(host.dataset.format,'visual');assert.equal(buttons.visualButton.attributes['aria-pressed'],'true');
 // The rendered pane shows only in Side by side, mounted on the same view.
 assert.equal(pane.hidden,true);assert.equal(panes.length,2);assert(panes.every(item=>item.view===created[0].view));
 assert(panes.every(item=>item.destroyed));
 editor.choose('split');await settle(editor);
 assert.equal(pane.hidden,false);assert.equal(panes.at(-1).destroyed,false);assert.equal(buttons.splitButton.attributes['aria-pressed'],'true');
 // Every note opens in the mode chosen last, here and after a reload.
 editor.value='Another note.\n';await settle(editor);
 assert.equal(created.length,2);assert.equal(created[1].format,'split');assert(panes.at(-2).destroyed);
 assert.equal(rememberedFormat(storage),'split');
 assert.equal(setup({storage}).editor.format,'split');
 assert.equal(rememberedFormat(memory({'fmn:editor-format':'wysiwyg'})),'visual');
 assert.equal(rememberedFormat({getItem(){throw Error('blocked')}}),'visual');
});
test('Side by side shows Source on a narrow screen and returns when it is wide again',async()=>{
 let wide=false;
 const {editor,shown,buttons,pane}=setup({wide:()=>wide});
 editor.value='Text.\n';editor.choose('split');editor.show(true);await settle(editor);
 assert.equal(editor.format,'split');assert.equal(shown().format,'source');assert.equal(pane.hidden,true);
 assert.equal(buttons.sourceButton.attributes['aria-pressed'],'true');
 wide=true;editor.render();await settle(editor);
 assert.equal(shown().format,'split');assert.equal(pane.hidden,false);
 assert.equal(shownFormat('split',false),'source');assert.equal(shownFormat('visual',false),'visual');
});
test('the view starts with whether the note is a private draft and follows changes while it shows that note',async()=>{
 let isPrivate=true;
 const {editor,shown}=setup({privateDraft:()=>isPrivate});
 editor.value='[Video](https://youtu.be/M7lc1UVf-VE "video")\n';editor.show(true);await settle(editor);
 const first=shown();
 assert.equal(first.privateDraft,true);
 // The view asks the app's media rules which host a player would contact.
 assert.equal(first.videoHost('https://youtu.be/M7lc1UVf-VE'),'www.youtube-nocookie.com');
 assert.equal(first.videoHost('/assets/clip.mp4'),null);
 // Saving the draft (or a refused live update making it private) tells the view.
 isPrivate=false;editor.privateDraftChanged();
 assert.deepEqual(first.privateDrafts,[false]);
 // The view of the note being replaced never takes the next note's state:
 // the next view starts with it.
 isPrivate=true;editor.value='Next note\n';editor.privateDraftChanged();
 assert.deepEqual(first.privateDrafts,[false]);
 await settle(editor);
 assert.notEqual(shown(),first);assert.equal(shown().privateDraft,true);assert.deepEqual(shown().privateDrafts,[]);
 // Without the app's answer the view shows saved notes.
 const plain=setup();plain.editor.value='x';plain.editor.show(true);await settle(plain.editor);
 assert.equal(plain.shown().privateDraft,false);
});
test('a slow editor load keeps the latest choice and mounts one view',async()=>{
 let release;const held=new Promise(resolve=>release=resolve),created=[];
 const view=({format})=>{const self={format,setFormat(next){self.format=next},apply(){},readonly(){},focus(){},select(){},selection:()=>({start:0,end:0,direction:'none'}),setComments(){},destroy(){}};created.push(self);return self};
 const load={editor:()=>held.then(()=>({createSource:view})),preview:async()=>({mountPreview:()=>({destroy(){}})})};
 const editor=new MarkdownEditor({host:{},root:element(),pane:element(),toolbar:element(),visualButton:element(),sourceButton:element(),hint:element(),load,storage:memory(),wide:()=>true});
 editor.value='text';editor.show(true);editor.choose('source');editor.choose('visual');editor.choose('source');
 assert.equal(editor.ready,false);
 release();await settle(editor);
 assert.equal(created.length,1);assert.equal(created[0].format,'source');assert(editor.ready);
});
test('a view that loads late takes focus only if nobody moved it meanwhile',async()=>{
 const saved=globalThis.document,body={},edit={},menu={};
 try{
  const source=element();
  for(const [moved,takes,label] of [[edit,true,'focus stayed on Edit'],[source,true,'focus moved to Source'],[body,true,'focus returned to the page'],[menu,false,'focus moved to a menu']]){
   let release;const held=new Promise(resolve=>release=resolve),focused=[];
   const view=()=>({setFormat(){},apply(){},readonly(){},focus(){focused.push(label)},select(){},selection:()=>({start:0,end:0,direction:'none'}),setComments(){},destroy(){}});
   const load={editor:()=>held.then(()=>({createSource:view})),preview:async()=>({mountPreview:()=>({destroy(){}})})};
   const editor=new MarkdownEditor({host:{},root:element(),pane:element(),toolbar:element(),visualButton:element(),sourceButton:source,hint:element(),load,storage:memory(),wide:()=>true});
   globalThis.document={body,activeElement:edit};
   editor.value='text';editor.show(true);
   globalThis.document.activeElement=moved;
   release();await settle(editor);
   assert(editor.ready,label);assert.deepEqual(focused,takes?[label]:[],label);
  }
 }finally{globalThis.document=saved}
});
test('selecting a range selects its offsets in every mode, also before the view loads',async()=>{
 for(const format of ['visual','source','split']){
  const {editor,shown}=setup();
  editor.value='One two three.\n';editor.choose(format);
  assert.equal(editor.selectRange('stale',0,3),false);assert.equal(editor.selectRange(editor.value,4,30),false);
  editor.show(true);assert.equal(editor.selectRange(editor.value,4,7),true);
  await settle(editor);
  assert.deepEqual(shown().selected,[4,7],format);assert.equal(shown().format,format);
  assert.equal(editor.selectRange(editor.value,8,13),true);assert.deepEqual(shown().selected,[8,13],format);
 }
});
test('comments reach the view; Show passage and Comment report what the view placed, in every mode',async()=>{
 for(const format of ['visual','source','split']){
  const {editor,shown}=setup();
  editor.value='Placed text.\n';editor.choose(format);editor.setComments([{id:'a',anchor:{quote:'placed'}},{id:'b',anchor:{quote:'elsewhere'}}]);
  assert.equal(editor.focusComment('a'),false,'a view that is not shown placed a passage');assert.equal(editor.commentAnchor(),null);
  editor.show(true);await settle(editor);
  assert.equal(shown().comments.length,2);
  assert.equal(editor.focusComment('a'),true,format);assert.deepEqual(shown().focused,['a',{quote:'placed'}]);
  assert.equal(editor.focusComment('b'),false);assert.equal(editor.focusComment('missing'),false);
  assert.deepEqual(editor.commentAnchor(),{quote:'selected'});
 }
});
// The input method owns an open composition's text: an undo dispatched into
// it changes text the live note does not hold yet, and the browser may never
// end the composition. Nothing reaches the view until it has ended.
test('Undo and Redo do nothing inside an open IME composition, locally or through the live session',async()=>{
 for(const flag of ['composing','compositionStarted']){
  let liveActive=false;const runs=[];
  const live={active:()=>liveActive,run:redo=>runs.push(redo),enabled:()=>true,boundary(){}};
  const {editor,shown,inputs}=setup({live});
  editor.value='abc';editor.choose('source');editor.show(true);await settle(editor);
  const view=shown(),calls=[];
  view.type([{from:3,to:3,insert:'d'}]);
  view.takeInput=()=>calls.push('takeInput');view.refresh=()=>calls.push('refresh');
  view.view[flag]=true;
  for(const active of [false,true]){liveActive=active;editor.undo();editor.undo(true)}
  assert.deepEqual(runs,[],flag);assert.deepEqual(calls,[],flag);assert.deepEqual(view.revealed,[],flag);
  assert.equal(editor.value,'abcd',flag);assert.equal(view.text,'abcd',flag);assert.equal(inputs.length,1,flag);
  // Once it has ended, they work again.
  view.view[flag]=false;
  liveActive=false;editor.undo();assert.equal(editor.value,'abc',flag);
  liveActive=true;editor.undo(true);assert.deepEqual(runs,[true],flag);
 }
});
// The rest of the calls that reach the view (see 'every call that reaches
// the view reads its unread input first').
test('closing the view and a screen-width change read the view’s unread input first, once',async()=>{
 const listeners=[],saved=globalThis.matchMedia;let wide=true;
 globalThis.matchMedia=()=>({get matches(){return wide},addEventListener:(_type,listener)=>listeners.push(listener)});
 try{
  const {created,load}=views(),inputs=[];
  const editor=new MarkdownEditor({host:{dataset:{}},root:element(),pane:element(),toolbar:element(),visualButton:element(),sourceButton:element(),splitButton:element(),hint:element(),load,storage:memory()});
  editor.addEventListener('input',event=>inputs.push(event.changes));
  editor.value='Local.\n';editor.choose('split');editor.show(true);await settle(editor);
  assert.equal(listeners.length,1);
  const view=created.at(-1),order=[],{setFormat,destroy}=view;
  view.takeInput=()=>{const changes=view.unread;view.unread=null;if(changes){order.push('input');view.type(changes)}};
  view.setFormat=next=>{order.push('setFormat '+next);setFormat(next)};
  view.destroy=()=>{order.push('destroy');destroy()};
  const typing=insert=>{const at=view.text.length-1;view.unread=[{from:at,to:at,insert}]};
  // The screen became narrow: Side by side shows Source.
  wide=false;typing(' narrow');for(const listener of listeners)listener();
  assert.deepEqual(order,['input','setFormat source']);assert.deepEqual(inputs,[[{from:6,to:6,insert:' narrow'}]]);
  editor.takeInput();assert.equal(inputs.length,1,'input reported twice');
  typing(' closed');editor.dispose();
  assert.deepEqual(order,['input','setFormat source','input','destroy']);assert.deepEqual(inputs.at(-1),[{from:13,to:13,insert:' closed'}]);
  assert.equal(editor.value,'Local. narrow closed\n');
  editor.takeInput();assert.equal(inputs.length,2,'input reported twice');
 }finally{if(saved)globalThis.matchMedia=saved;else delete globalThis.matchMedia}
});
// The app has left a note's live session and draft before it sets the next
// note's text, so input the old view has not read cannot reach them from
// here; reported now, it would be an edit of the next note.
test('replacing the note never reports the old view’s unread input as an edit of the new one',async()=>{
 const {editor,shown,inputs}=setup();
 editor.value='Old note.\n';editor.show(true);await settle(editor);
 const view=shown();
 view.unread=[{from:9,to:9,insert:' unread'}];
 view.takeInput=()=>{const changes=view.unread;view.unread=null;if(changes)view.type(changes)};
 editor.value='New note.\n';await settle(editor);
 assert.deepEqual(inputs,[]);assert.equal(editor.value,'New note.\n');assert.equal(shown().text,'New note.\n');
});
test('a read-only editor undoes nothing, locally or through the live session',async()=>{
 let liveActive=false;const runs=[];
 const live={active:()=>liveActive,run:redo=>runs.push(redo),enabled:()=>true,boundary(){}};
 const {editor,shown,inputs}=setup({live});
 editor.value='abc';editor.choose('source');editor.show(true);await settle(editor);
 shown().type([{from:3,to:3,insert:'d'}]);
 editor.readOnly=true;
 assert.equal(editor.canUndo(false),false);editor.undo();
 assert.equal(editor.value,'abcd');assert.equal(shown().text,'abcd');assert.equal(inputs.length,1);
 liveActive=true;editor.undo();editor.undo(true);assert.deepEqual(runs,[]);
 editor.readOnly=false;liveActive=false;
 assert.equal(editor.canUndo(false),true);editor.undo();assert.equal(editor.value,'abc');
});
test('a view that cannot show a change is rebuilt from the note',async()=>{
 const {editor,created,inputs}=setup();
 editor.value='One.\n\nTwo.\n';editor.show(true);await settle(editor);
 const old=created[0];old.refuse=true;
 editor.applyRemote([{from:0,to:0,insert:'> '}]);
 await settle(editor);
 assert(old.destroyed);assert.equal(created.length,2);assert.equal(created[1].text,'> One.\n\nTwo.\n');
 assert.equal(editor.value,'> One.\n\nTwo.\n');assert.deepEqual(inputs,[]);
 assert.equal(editor.hint.textContent,'');
});
test('Undo shows where it changed the note; other changes do not move the view',async()=>{
 for(const format of ['source','visual','split']){
  let editor,liveActive=false;
  // The session's undo reaches the editor as a remote change, synchronously.
  const live={active:()=>liveActive,enabled:()=>true,boundary(){},run:redo=>editor.applyRemote(redo?[{from:0,to:0,insert:'Z'}]:[{from:6,to:6,insert:'back '}])};
  const setupResult=setup({live});editor=setupResult.editor;
  editor.value='Hello world.\n';editor.choose(format);editor.show(true);await settle(editor);
  const view=setupResult.shown();
  view.type([{from:5,to:5,insert:' there'}]);
  editor.undo();assert.deepEqual(view.revealed,[5],format);
  editor.undo(true);assert.deepEqual(view.revealed,[5,11],format);
  editor.applyRemote([{from:0,to:0,insert:'>'}]);assert.deepEqual(view.revealed,[5,11],format);
  liveActive=true;editor.undo();editor.undo(true);
  assert.deepEqual(view.revealed,[5,11,11,1],format);
  assert.equal(editor.value,'Z>Helloback  there world.\n',format);
 }
});
