import {videoHost,videoPlayer} from './media.mjs';
import {TextPieces} from './text-pieces.mjs';

// Text changes {from, to, insert} apply in order, each to the text the one
// before it left. Returns the new text and the changes that undo them.
export function applyChanges(text, changes) {
  const inverse=[];
  for(const {from,to,insert} of changes){
    if(!Number.isInteger(from) || !Number.isInteger(to) || from<0 || to<from || to>text.length || typeof insert!=='string')
      throw new RangeError('Invalid text change '+JSON.stringify({from,to})+' for length '+text.length);
    inverse.push({from,to:from+insert.length,insert:text.slice(from,to)});
    text=text.slice(0,from)+insert+text.slice(to);
  }
  return {text,inverse:inverse.reverse()};
}
// The one change turning `before` into `after`, never splitting a surrogate pair.
export function interval(before,after) {
  let start=0,end=before.length,nextEnd=after.length;
  while(start<end && start<nextEnd && before.charCodeAt(start)===after.charCodeAt(start))start++;
  if(start>0 && /[\uDC00-\uDFFF]/.test((before[start]||'')+(after[start]||'')))start--;
  while(end>start && nextEnd>start && before.charCodeAt(end-1)===after.charCodeAt(nextEnd-1)){end--;nextEnd--}
  if(end<before.length && /[\uDC00-\uDFFF]/.test(before[end])){end++;nextEnd++}
  return {from:start,to:end,insert:after.slice(start,nextEnd)};
}
// Undo for this user's edits before a live session joins; the session's undo
// manager owns undo while it is connected. Edits within `delay` ms join one
// step unless a boundary separates them.
export class TextHistory {
  constructor({delay=500,now=()=>Date.now()}={}){Object.assign(this,{delay,now});this.clear()}
  clear(){this.done=[];this.undone=[];this.last=-Infinity;this.split=true}
  boundary(){this.split=true}
  record(changes,inverse){
    if(!changes.length)return;
    const time=this.now(),top=this.done.at(-1);
    if(top && !this.split && time-this.last<this.delay){top.changes.push(...changes);top.inverse.unshift(...inverse)}
    else this.done.push({changes:[...changes],inverse:[...inverse]});
    this.undone=[];this.last=time;this.split=false;
  }
  can(redo){return (redo?this.undone:this.done).length>0}
  // The changes that undo (or redo) the last step.
  take(redo){
    const step=(redo?this.undone:this.done).pop();if(!step)return null;
    (redo?this.done:this.undone).push(step);this.split=true;
    return redo?step.changes:step.inverse;
  }
}

// Editing modes: Visual (the live preview), Source (plain Markdown) and Side
// by side (Source next to the rendered note).
export const FORMATS=['visual','source','split'];
// Decision 1: every note opens in the mode this browser chose last, Visual
// at first. Side by side needs a wide screen (decision 2); narrower screens
// show Source instead until the screen is wide again.
const FORMAT_KEY='fmn:editor-format';
export const SPLIT_MIN_WIDTH=900;
export function rememberedFormat(storage) {
  try{const value=storage?.getItem(FORMAT_KEY);return FORMATS.includes(value)?value:'visual'}catch{return 'visual'}
}
function rememberFormat(storage,format) {
  try{storage?.setItem(FORMAT_KEY,format)}catch{}
}
export function shownFormat(format,wide) {return format==='split' && !wide?'source':format;}
const defaultStorage=()=>{try{return globalThis.localStorage}catch{return null}};
const defaultWide=()=>typeof matchMedia!=='function' || matchMedia('(min-width: '+SPLIT_MIN_WIDTH+'px)').matches;

// The note editor. It owns the note's Markdown text; one CodeMirror view per
// note shows it in every mode and reports its edits as text changes. Every
// edit is one `input` event carrying its `changes`; changes made elsewhere
// (peers, agents, live undo) arrive through applyRemote() and are never
// reported back. Switching modes reconfigures the view: it never remounts it
// or changes the text. The view loads lazily and reads the current text when
// it is created, and whether that text is a private draft (privateDraft()),
// whose outside videos Visual does not load until the user asks.
//
// The browser changes the view's text before the view reads the change, and
// a dispatch to the view in between throws the change away. So every call
// that reaches the view, or acts on offsets of its text, first reads that
// input (takeInput), which arrives as this user's edit; the live session asks
// for it before it applies a change made elsewhere (applyRemote), whose
// offsets are those of the note with this user's input.
export class MarkdownEditor extends EventTarget {
  constructor({host,root,pane,toolbar,visualButton,sourceButton,splitButton,hint,imageURL,resolveLink,commentSelection,openThread,insertMedia,openNoteLink,openLink,pageTarget,liveHistory,
    privateDraft=()=>false,storage=defaultStorage(),wide=defaultWide,
    load={editor:()=>import('./dist/source-editor.js'),preview:()=>import('./dist/rendered-preview.js')}}) {
    super();
    Object.assign(this,{host,root,pane,toolbar,visualButton,sourceButton,splitButton,hint,imageURL,resolveLink,commentSelection,openThread,insertMedia,openNoteLink,openLink,pageTarget,liveHistory,privateDraft,storage,wide,load});
    // The note's text, in pieces, so a keystroke in a long note does not copy it.
    this.pieces=new TextPieces();
    this.preferredFormat=rememberedFormat(storage);
    this.format=this.preferredFormat;
    this.version=0;
    this.shown=false;
    this.locked=false;
    this.rendered=Promise.resolve();
    this.history=new TextHistory();
    this.caret={start:0,end:0,direction:'none'};
    // The toolbar's Undo and Redo, and the boundaries that make a command one
    // undo step of its own.
    this.undoControls={run:redo=>{this.undo(redo);return true},enabled:redo=>this.canUndo(redo),
      boundary:()=>{this.liveHistory?.boundary?.();this.history.boundary();}};
    visualButton.onclick=()=>this.choose('visual');
    sourceButton.onclick=()=>this.choose('source');
    if(splitButton)splitButton.onclick=()=>this.choose('split');
    if(typeof matchMedia==='function' && wide===defaultWide)matchMedia('(min-width: '+SPLIT_MIN_WIDTH+'px)').addEventListener?.('change',()=>this.render());
    // The Markdown has no form field; automation reads it here.
    if(host)Object.defineProperty(host,'markdown',{configurable:true,get:()=>this.text});
  }
  get text() {return this.pieces.text;}
  set text(value) {this.pieces.reset(value);}
  get value() {return this.text;}
  // The app leaves the shown note's live session and draft before it sets
  // the next note's text, so input the old view has not read is not read
  // here: it would arrive as an edit of the next note.
  set value(value) {
    this.text=value;this.history.clear();this.caret={start:0,end:0,direction:'none'};this.format=this.preferredFormat;
    this.version++;this.render();
  }
  get readOnly() {return this.locked;}
  set readOnly(value) {this.locked=!!value;this.takeInput();this.view?.readonly(this.locked);}
  // Whether the view is loaded and shows this text.
  get ready() {return !!this.view && this.loaded===this.version;}
  // The mode shown: Side by side only on a wide screen.
  get shownFormat() {return shownFormat(this.format,this.wide());}
  selection() {
    this.takeInput();
    if(this.ready)this.caret=this.view.selection();
    return this.caret;
  }
  get selectionStart() {return this.selection().start;}
  get selectionEnd() {return this.selection().end;}
  live() {return !!this.liveHistory?.active?.();}
  // This user's edit: the text, the undo step, one input event, then the
  // view (unless the view made it).
  edited(changes,fromView=false) {
    if(!changes.length)return;
    const inverse=this.pieces.apply(changes);
    if(!this.live())this.history.record(changes,inverse);
    this.dispatchEvent(Object.assign(new Event('input'),{changes}));
    if(!fromView)this.present(changes);
  }
  // Changes made elsewhere: peers, agents, live undo. Their offsets include
  // this user's input, which the session read first (takeInput): read here,
  // it would land at offsets of the note without the change.
  applyRemote(changes) {
    if(!changes.length)return;
    this.pieces.apply(changes);
    this.history.clear();
    this.present(changes);
    this.undoing?.push(changes);
  }
  // Reports text the browser changed that the view has not read yet, as this
  // user's edit (an input event), so no remote change can overtake it.
  takeInput() {if(this.ready)this.view.takeInput?.();}
  // Moves the editor to the text `source` holds (a string, or the live text),
  // with one change, as a remote change: used once when a live session joins
  // with text the editor does not show yet. Input read first reaches the
  // session, so the source is read after it.
  sync(source) {
    this.takeInput();
    const text=String(source);
    if(text!==this.text)this.applyRemote([interval(this.text,text)]);
  }
  // Shows changes the view did not make; a view that cannot is rebuilt from the text.
  present(changes) {
    if(!this.ready)return;
    try{this.view.apply(changes)}
    catch(error){this.message(error.message);this.version++;this.render()}
  }
  // The view's own edit, unless it comes from a view this editor no longer shows.
  viewEdit(version) {
    return changes=>{
      if(version!==this.version)return;
      try{this.edited(changes,true)}
      catch(error){this.message(error.message);this.version++;this.render()}
    };
  }
  // Undo and Redo change the note, so a read-only editor refuses them, and
  // so does an open IME composition, whose text the input method owns:
  // nothing may reach the view until it ends (as with every command).
  undo(redo=false) {
    if(this.readOnly || this.composing())return;
    // Typing the view has not read yet is the step to undo.
    this.takeInput();
    // The session's undo reaches the editor as a remote change while it
    // runs; its undo steps change after that change reached the view.
    if(this.live()){
      const applied=this.undoing=[];
      try{this.liveHistory.run(redo)}finally{this.undoing=null}
      if(this.ready)this.view.refresh?.();
      this.reveal(applied.at(-1) ?? []);
      return;
    }
    const changes=this.history.take(redo);if(!changes)return;
    this.pieces.apply(changes);
    this.dispatchEvent(Object.assign(new Event('input'),{changes}));
    this.present(changes);
    this.reveal(changes);
  }
  // Shows where an undo changed the note (the end of its last change), which
  // may be off screen, in every mode.
  reveal(changes) {
    this.takeInput();
    const last=changes.at(-1);
    if(last && this.ready)this.view.reveal(last.from+last.insert.length);
  }
  composing() {const view=this.view?.view;return !!(view && (view.composing || view.compositionStarted));}
  canUndo(redo=false) {return !this.readOnly && (this.liveHistory?.enabled?.(redo) ?? this.history.can(redo));}
  setRangeText(text,start,end,mode) {
    this.takeInput();
    if(this.ready)this.view.replace(text,start,end,mode);
    else {
      this.edited([{from:start,to:end,insert:text}]);
      const at=mode==='start'?start:start+text.length;this.caret={start:mode==='select'?start:at,end:at,direction:'none'};
    }
  }
  // A programmatic fix of exact text (writing tools): one undo step of its own.
  replaceRange(expected,from,to,text) {
    this.takeInput();
    if(this.readOnly || this.value!==expected || !Number.isInteger(from)||!Number.isInteger(to)||from<0||to<from||to>expected.length)return false;
    this.liveHistory?.boundary?.();this.history.boundary();
    try {this.edited([{from,to,insert:text}]);return true;}
    finally {this.liveHistory?.boundary?.();this.history.boundary();}
  }
  // Selects [from, to) of the text in whatever mode is shown.
  selectRange(expected,from,to) {
    this.takeInput();
    if(this.value!==expected || !Number.isInteger(from) || !Number.isInteger(to) || from<0 || to<from || to>expected.length)return false;
    this.caret={start:from,end:to,direction:'forward'};
    if(this.ready){this.view.focus();this.view.select(from,to);return true;}
    // The view loads lazily; it selects the range once it shows this text.
    this.rendered.then(()=>{if(this.ready && this.value===expected){this.view.focus();this.view.select(from,to)}});
    return true;
  }
  show(value) {this.shown=value;if(value)this.render();}
  // Focusing the view stops and restarts its DOM observer, which drops
  // what the observer has not reported.
  focus() {this.takeInput();if(this.ready)this.view.focus();}
  choose(format) {
    if(!FORMATS.includes(format))return;
    this.takeInput();
    this.preferredFormat=format;rememberFormat(this.storage,format);
    if(this.format===format)return;
    this.format=format;this.render();
  }
  setComments(comments) {this.comments=comments;this.takeInput();if(this.ready)this.view.setComments(comments);}
  commentAnchor() {this.takeInput();return this.ready ? this.view.commentAnchor() : null;}
  // Selects a comment's passage; false if the editor is not shown or cannot
  // place it.
  focusComment(id) {
    this.takeInput();
    const anchor=this.comments?.find(comment=>comment.id===id)?.anchor;
    return !!(this.ready && anchor && this.view.focusComment(id,anchor));
  }
  message(text) {this.hint.textContent=text;this.hint.hidden=!text;}
  // The note became a private draft or was saved (privateDraft() says which).
  // A view is told only while it shows this note's text: a note being
  // replaced keeps its own, and the next view starts with the new one.
  privateDraftChanged() {this.takeInput();if(this.ready)this.view.setPrivateDraft(!!this.privateDraft());}
  dispose() {
    this.takeInput();
    this.closePane();
    if(this.view){
      if(this.loaded===this.version)this.caret=this.view.selection();
      this.view.destroy();this.view=null;
    }
    this.loaded=null;
    this.root.replaceChildren();
  }
  closePane() {
    this.paneVersion=null;
    if(this.paneView){this.paneView.destroy();this.paneView=null;}
    if(this.pane){this.pane.replaceChildren();this.pane.hidden=true;}
  }
  // Side by side's rendered pane, loaded the first time it shows.
  openPane() {
    if(!this.pane || this.paneVersion===this.version)return;
    const version=this.paneVersion=this.version,view=this.view;
    this.pane.hidden=false;
    Promise.resolve(this.load.preview()).then(({mountPreview})=>{
      if(this.paneVersion!==version || this.view!==view || this.paneView)return;
      this.paneView=mountPreview({host:this.pane,view:view.view,imageURL:this.imageURL,resolve:this.resolveLink,open:this.openLink,pageTarget:this.pageTarget});
    }).catch(error=>{if(this.paneVersion===version)this.message(error.message)});
  }
  render() {
    if(!this.shown)return;
    const format=this.shownFormat;
    for(const [button,name] of [[this.visualButton,'visual'],[this.sourceButton,'source'],[this.splitButton,'split']])button?.setAttribute('aria-pressed',String(format===name));
    if(this.host?.dataset)this.host.dataset.format=format;
    if(this.ready){
      this.takeInput();
      this.view.setFormat(format);
      if(format==='split')this.openPane();else this.closePane();
      return;
    }
    if(this.loading===this.version)return;
    const version=this.loading=this.version;
    this.message('Loading editor…');
    const focused=globalThis.document?.activeElement;
    this.rendered=Promise.resolve(this.load.editor()).then(module=>{
      if(version!==this.version || !this.shown)return;
      this.dispose();
      this.mount(version,module,focused);
      this.render();
    }).catch(error=>{if(version===this.version)this.message(error.message)})
      .finally(()=>{if(this.loading===version)this.loading=null;});
  }
  // The view takes focus unless someone moved it elsewhere while the view
  // loaded, for example to a menu or the search field. Choosing a format is
  // not elsewhere, and focus in the view it replaces returns to the page.
  mount(version,{createSource},focused) {
    const view=createSource({root:this.root,text:this.text,readOnly:this.readOnly,format:this.shownFormat,changed:this.viewEdit(version),
      imageURL:this.imageURL,video:href=>videoPlayer(href,path=>this.imageURL?.(path)),privateDraft:!!this.privateDraft(),videoHost,message:text=>this.message(text),
      history:this.undoControls,toolbar:this.toolbar,insertMedia:this.insertMedia,commentSelection:this.commentSelection,openNoteLink:this.openNoteLink,
      openThread:this.openThread,openLink:this.openLink,pageTarget:this.pageTarget});
    this.view=view;this.loaded=version;
    const {start,end}=this.caret,length=this.text.length;
    if(start || end)view.select(Math.min(start,length),Math.min(end,length));
    view.setComments(this.comments || []);
    this.message('');
    const page=globalThis.document,active=page?.activeElement;
    const kept=!active || active===page.body || active===focused || [this.visualButton,this.sourceButton,this.splitButton].includes(active);
    if(this.shown && kept)view.focus();
  }
}
