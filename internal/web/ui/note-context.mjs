import {$,element,contentFragment,routeLink} from './dom.mjs';
import {setLabel} from './icons.mjs';
import {timeElement} from './time.mjs';
import {overlays} from './overlay.mjs';
import {feedback,confirmAction} from './feedback.mjs';
import {requestSequence} from './state.mjs';
import {selectionAnchor,readingRanges} from './annotations.mjs';
import {notebookPath} from './request.mjs';
import {noteOutline} from './note-outline.mjs';
import {paced} from './paced.mjs';

// The note context panel (contents, references, comments) and passage comments.
// It owns discussion and composer state; the caller owns the note, edit mode and
// saves (`beginWrite`/`endWrite`), and is told when the preview has re-rendered,
// when edit mode changes and when the text changes while editing.
// Reading, Contents and Show passage read the rendered preview. While editing,
// the hidden preview is not rendered (a whole-note upload and render at every
// pause on a novel, plan risk 18): Contents reads the editor's text and Show
// passage asks the editor; `previewStale()` says the preview predates edits.
export function initNoteContext({api,post,notice,getContext,noteEditor,setMode,dirty,canManage,canEdit,beginWrite,endWrite,refreshStatus,references,onRoute,previewStale}) {
  let discussion=null,contextRequests=requestSequence(),contextOpener=null;
  let pendingAnchor=null,replyTo="",readingCommentRanges=new Map(),commentRequests=requestSequence();
  let shownEditing=false,editorOutline=null,passageShown=null;
  const contextOverlay=matchMedia('(max-width: 1279px)');
  function updateContextOverlay(){
   const panel=$('comments-panel'),modal=!panel.hidden && contextOverlay.matches;
   $('context-backdrop').hidden=!modal;
   if(modal){panel.setAttribute('role','dialog');panel.setAttribute('aria-modal','true');}
   else {panel.removeAttribute('role');panel.removeAttribute('aria-modal');}
   overlays().panel(panel,{active:!panel.hidden,modal,group:'note-panels',close:closeContext,covers:[$('main'),$('sidebar'),$('mobile-navigation')]});
  }
  contextOverlay.addEventListener('change',updateContextOverlay);

  function setCommentPanel(open) {
    contextRequests.next();
    $("comments-panel").hidden=!open;
    updateContextOverlay();
    document.body.classList.toggle("comments-open",open);
    $("comments-open").setAttribute("aria-expanded",String(open));
    $("note-context-toggle").setAttribute("aria-expanded",String(open));
    if(!open)$("note-context-references").replaceChildren();
    else if(getContext().editing)refreshPanel();
  }
  function refreshPanel(){renderContents();if(discussion)renderComments();}
  // While the panel is open, Contents follows the text once typing pauses.
  const editorContents=paced(()=>{if(getContext().editing && !$("comments-panel").hidden)renderContents();});
  let contentsFrame;
  function markCurrentHeading(){
   // The preview's headings are hidden while editing.
   if(getContext().editing)return;
   const headings=[...$("preview").querySelectorAll("h1[id],h2[id],h3[id],h4[id],h5[id],h6[id]")];
   const clearance=document.querySelector('#main > header').getBoundingClientRect().bottom+24;
   const active=headings.findLast(heading=>heading.getBoundingClientRect().top<=clearance) || headings[0];
   for(const link of $("note-context-contents").querySelectorAll('a')){
    if(active && link.hash==='#'+encodeURIComponent(contentFragment(active.id)))link.setAttribute('aria-current','location');else link.removeAttribute('aria-current');
   }
  }
  addEventListener('scroll',()=>{if(contentsFrame || $('comments-panel').hidden)return;contentsFrame=requestAnimationFrame(()=>{contentsFrame=0;markCurrentHeading();});},{passive:true});
  let hasContents=false;
  function renderContents(){
   if(getContext().editing)return renderEditorContents();
   editorOutline=null;
   const all=[...$("preview").querySelectorAll("h1[id],h2[id],h3[id],h4[id],h5[id],h6[id]")];
   // The leading title is already the page heading; Contents lists its sections.
   const headings=all[0]?.tagName==="H1" && all[0]===$("preview").firstElementChild?all.slice(1):all;
   const top=Math.min(...headings.map(heading=>Number(heading.tagName[1])));
   hasContents=all.length>=2;
   document.querySelector('[data-context-tab="contents"]').hidden=!hasContents;
   $("note-context-contents").replaceChildren(...headings.map(heading=>{
    const link=document.createElement("a");link.textContent=heading.textContent;link.href="#"+encodeURIComponent(contentFragment(heading.id));link.style.paddingLeft=(12+(Number(heading.tagName[1])-top)*12)+"px";
    return routeLink(link,()=>{if(contextOverlay.matches)setCommentPanel(false);heading.scrollIntoView({block:"start"});heading.tabIndex=-1;heading.focus({preventScroll:true});history.replaceState(history.state,"",link.href);onRoute();markCurrentHeading();});
   }));
  }
  // Contents of the editor's text. The links stay while the headings do,
  // and lead to their place in the latest text read.
  function renderEditorContents(text=noteEditor.value){
   const {title,headings}=noteOutline(text),shape=headings.map(heading=>heading.level+' '+heading.text).join('\n');
   const drawn=editorOutline?.shape===shape;
   editorOutline={text,headings,shape};
   hasContents=headings.length+(title?1:0)>=2;
   document.querySelector('[data-context-tab="contents"]').hidden=!hasContents;
   if(drawn)return;
   const top=Math.min(...headings.map(heading=>heading.level));
   $("note-context-contents").replaceChildren(...headings.map((heading,index)=>{
    const link=document.createElement("a");link.textContent=heading.text;link.href="#";link.style.paddingLeft=(12+(heading.level-top)*12)+"px";
    return routeLink(link,()=>goToHeading(index));
   }));
  }
  // Puts the caret on the heading's line, the same heading in the text typed
  // since Contents was drawn.
  function goToHeading(index){
   if(!getContext().editing || !editorOutline)return;
   const drawn=editorOutline.headings[index],text=noteEditor.value;
   let headings=editorOutline.headings,heading=drawn;
   if(text!==editorOutline.text){
    const same=other=>other.level===drawn.level && other.text===drawn.text,nth=headings.slice(0,index).filter(same).length;
    renderEditorContents(text);headings=editorOutline.headings;heading=headings.filter(same)[nth];
    if(!heading)return;
   }
   if(contextOverlay.matches)setCommentPanel(false);
   noteEditor.selectRange(text,heading.from,heading.from);
   [...$("note-context-contents").querySelectorAll('a')].forEach((link,at)=>{if(at===headings.indexOf(heading))link.setAttribute('aria-current','location');else link.removeAttribute('aria-current');});
  }
  function chooseContext(tab){
   contextRequests.next();
   if(tab==='contents')markCurrentHeading();
   for(const button of document.querySelectorAll('[data-context-tab]'))button.setAttribute('aria-pressed',String(button.dataset.contextTab===tab));
   for(const name of ['contents','references','comments'])$('note-context-'+name).hidden=name!==tab;
   if(tab==='references'){
    const request=contextRequests.current(),note=getContext().current,account=getContext().user?.id,id=getContext().project?.id;
    references($('note-context-references'),()=>contextRequests.owns(request) && getContext().current===note && getContext().user?.id===account && getContext().project?.id===id && !$('comments-panel').hidden,()=>setCommentPanel(false),count=>{referenceCount=count;renderContextCounts();});
   }
  }
  function openContext(tab,opener=$("note-details-toggle")){
   if(!getContext().current)return;
   contextOpener=opener;setCommentPanel(true);chooseContext(tab);
   document.querySelector('[data-context-tab="'+tab+'"]').focus({preventScroll:true});
  }
  function closeContext(){setCommentPanel(false);contextOpener?.focus({preventScroll:true});}
  $('context-backdrop').onclick=closeContext;
  function toggleContext(opener){
   if(!$('comments-panel').hidden)return closeContext();
   // Headings typed since the preview last rendered count.
   if(getContext().editing)renderContents();
   if(!hasContents)openComments(undefined,opener);else openContext('contents',opener);
  }
  $('note-context-toggle').onclick=()=>toggleContext($('note-context-toggle'));
  let referenceCount=null;
  function renderContextCounts(){
   const comments=discussion?.comments.length;
   // Zero counts are noise in the top bar; the panel itself shows empty states.
   const details=[comments?comments+' '+(comments===1?'comment':'comments'):'',referenceCount?referenceCount+' '+(referenceCount===1?'reference':'references'):''].filter(Boolean);
   setLabel($('note-context-toggle'),details.length?'Context · '+details.join(' · '):'Context');
   $('note-context-toggle').setAttribute('aria-label','Note context'+(details.length?' · '+details.join(' · '):''));
  }

  for(const button of document.querySelectorAll('[data-context-tab]'))button.onclick=()=>button.dataset.contextTab==='comments'?openComments():chooseContext(button.dataset.contextTab);
  $('sources-open').onclick=()=>openContext('references');

  function resetComposer() {
    pendingAnchor=null;replyTo="";
    $("comment-context").hidden=true;$("comment-context").textContent="";
    $("comment-reset").hidden=true;$("comment-label").textContent="Add a comment";
  }
  function resetComments() {
    setCommentPanel(false);resetComposer();discussion=null;referenceCount=null;editorOutline=null;passageShown=null;renderContextCounts();commentRequests.next();$("comments-list").replaceChildren();$("comments-note").textContent="";
    $("comment-text").value="";$("comment-text").disabled=false;$("comment-status").textContent="";
    noteEditor.setComments([]);paintComments();
  }
  function paintComments() {
    readingCommentRanges=readingRanges($("preview"),discussion?.comments || []);
  }
  function hasPassage(id) {
    return readingCommentRanges.has(id);
  }
  // The editor selects the passage in every mode; one it has not placed is
  // shown in the reading view, once that has rendered the text edited.
  function focusPassage(id) {
    if(getContext().editing){
      if(noteEditor.focusComment(id))return;
      const rendering=previewStale();
      setMode(false);
      if(rendering){passageShown=id;return;}
    }
    showReadingPassage(id);
  }
  function showReadingPassage(id) {
    readingCommentRanges.get(id)?.startContainer.parentElement?.scrollIntoView({block:'center',behavior:'smooth'});
  }
  function renderComments() {
    $("comments-list").replaceChildren();
    $("comment-form").hidden=getContext().project.role==='read';
    if(!discussion?.comments.length) {
      const empty=document.createElement('p');empty.className='small';empty.textContent='No comments yet.';$("comments-list").append(empty);
    }
    for(const comment of discussion?.comments || []) {
      const article=document.createElement('article');article.id='thread-'+comment.id;
      article.className='comment-thread'+(comment.resolved?' resolved':'');article.tabIndex=-1;
      function message(item,reply=false) {
        const element=document.createElement('div');element.className=reply?'comment-reply':'comment-message';
        const meta=document.createElement('div');meta.className='comment-meta';
        const name=discussion.people?.[item.author] || 'Contributor';
        const initial=document.createElement('span');initial.className='person-initial';initial.setAttribute('aria-hidden','true');initial.textContent=Array.from(name)[0] || '?';
        meta.append(initial,document.createTextNode(name+' · '),timeElement(item.created));
        const body=document.createElement('p');body.textContent=item.deleted?'Comment deleted':item.text;
        if(item.deleted)body.className='small';
        element.append(meta,body);
        const ownComment=['comment','edit','owner'].includes(getContext().project.role) && item.author===getContext().user.id;
        if(!item.deleted && (canManage() || ownComment)) {
          const remove=document.createElement('button');remove.type='button';remove.className='quiet comment-delete';remove.textContent=reply?'Delete reply':'Delete comment';
          remove.onclick=async()=>{
            const prompt=reply?'Delete this reply?':comment.replies?.length?'Delete this comment? Replies will remain.':'Delete this comment?';
            if(await confirmAction(prompt))commentMutation('/api/comments/delete',{id:comment.id,...(reply?{replyId:item.id}:{})});
          };element.append(remove);
        }
        article.append(element);
      }
      if(comment.anchor) {
        // While editing, the editor places the passage when asked.
        const placed=getContext().editing || hasPassage(comment.id);
        const quote=document.createElement('button');quote.className='comment-quote';quote.textContent=comment.anchor.quote;
        quote.disabled=comment.resolved || !placed;quote.title='Show passage';
        quote.onclick=()=>focusPassage(comment.id);article.append(quote);
        if(!comment.resolved && !placed) {
          const orphan=document.createElement('p');orphan.className='small';orphan.textContent='Passage changed or removed';article.append(orphan);
        }
      }
      message(comment);
      for(const reply of comment.replies || [])message(reply,true);
      if(comment.resolved) {
        const label=document.createElement('span');label.className='small';label.textContent='Resolved';article.append(label);
      } else if(getContext().project.role!=='read') {
        const actions=document.createElement('div');actions.className='comment-actions';
        const reply=document.createElement('button');reply.className='quiet';reply.textContent='Reply';
        reply.onclick=()=>{
          resetComposer();replyTo=comment.id;
          $("comment-context").textContent=comment.deleted?"Comment deleted":comment.text;$("comment-context").hidden=false;
          $("comment-reset").hidden=false;$("comment-label").textContent='Reply to '+(discussion.people?.[comment.author] || 'Contributor');$("comment-text").focus();
        };actions.append(reply);
        if(canEdit() || comment.author===getContext().user.id) {
          const resolve=document.createElement('button');resolve.className='quiet';resolve.textContent='Resolve';
          resolve.onclick=()=>commentMutation('/api/comments/resolve',{id:comment.id});actions.append(resolve);
        }
        article.append(actions);
      }
      $("comments-list").append(article);
    }
  }
  async function loadComments() {
    if(!getContext().current)return false;
    const note=getContext().current,projectID=getContext().project.id,request=commentRequests.next();
    const result=await api(notebookPath(getContext().project.id)+'/api/comments?path='+encodeURIComponent(note.path));
    if(getContext().current!==note || getContext().project.id!==projectID || !commentRequests.owns(request))return false;
    discussion={...result,path:note.path,project:projectID};
    $("comment-text").disabled=false;
    $("comments-note").textContent=note.path;
    noteEditor.setComments(discussion.comments);paintComments();renderComments();renderContextCounts();return true;
  }
  async function openComments(id,opener=$("note-details-toggle")) {
    if(getContext().saving || !getContext().current)return;
    if($("comments-panel").hidden)openContext("comments",opener);else chooseContext("comments");
    $("comment-text").disabled=true;
    $("comments-list").textContent="Loading comments…";$("comments-note").textContent=getContext().current.path;
    const request=contextRequests.current();
    try {
      if(!await loadComments() || !contextRequests.owns(request) || $("comments-panel").hidden)return;
      $("comment-text").disabled=false;
      $("comment-status").textContent='';
      if(id) {
        const thread=$('thread-'+id);thread?.scrollIntoView({block:'nearest'});thread?.focus({preventScroll:true});
      }
    } catch(e){if(contextRequests.owns(request)&&!$("comments-panel").hidden){
      $("comment-text").disabled=true;const message=element('p',e.message);feedback(message,e.message);const retry=element('button','Retry loading comments','quiet');retry.type='button';retry.onclick=()=>{if(contextRequests.owns(request)&&!$("comments-panel").hidden)openComments(undefined,contextOpener)};$("comments-list").replaceChildren(message,retry);
    }}
  }
  // The anchor may be a promise: the editor makes it from the whole note.
  async function startPassageComment(pending) {
    if(!pending || getContext().project.role==='read')return;
    const note=getContext().current,saved=()=>!dirty() || void notice('Save your draft before commenting on a passage.');
    if(!saved())return;
    let anchor;
    try{anchor=await pending;}catch(e){notice(e.message);return;}
    if(getContext().current!==note || !saved())return;
    if(!anchor?.quote)return void notice('The selection is markup that shows no text in the note, so there is nothing to comment on.');
    await openComments();
    if(getContext().current!==note || $("comments-panel").hidden)return;
    resetComposer();pendingAnchor=anchor;
    $("comment-context").textContent=anchor.quote;$("comment-context").hidden=false;
    $("comment-reset").hidden=false;$("comment-label").textContent='Comment on passage';$("comment-text").focus();
  }
  async function commentMutation(endpoint,body) {
    if(getContext().saving || !discussion)return;
    const note=getContext().current,projectID=getContext().project.id;
    beginWrite();
    try {
      await post(notebookPath(projectID)+endpoint,{...body,path:note.path,revision:discussion.revision});
      if(getContext().current!==note || getContext().project.id!==projectID)return;
      if(endpoint==='/api/comments'){resetComposer();$("comment-text").value='';}
      await loadComments();await refreshStatus();$("comment-status").textContent='Discussion saved.';
    } catch(e){$("comment-status").textContent=e.message+' Reopen Comments to refresh the discussion.';}
    finally{endWrite();}
  }
  $("comments-open").onclick=()=>openComments();
  $("comments-close").onclick=closeContext;
  $("comment-reset").onclick=resetComposer;
  $("comment-form").onsubmit=e=>{
    e.preventDefault();
    if(pendingAnchor && dirty()){$("comment-status").textContent='Save your draft before commenting on a passage.';return;}
    commentMutation('/api/comments',{text:$("comment-text").value,...(pendingAnchor?{anchor:pendingAnchor}:{}),...(replyTo?{replyTo}:{})});
  };
  $("comment-selection").onpointerdown=e=>e.preventDefault();
  $("comment-selection").onclick=()=>startPassageComment(selectionAnchor($("preview")));
  document.addEventListener('selectionchange',()=>{
    const available = !getContext().editing && getContext().current && getContext().project?.role !== 'read' && !!selectionAnchor($("preview"))?.quote;
    $("reading-actions").hidden = !available;
    $("comment-selection").disabled = !available;
  });
  $("preview").addEventListener('click',e=>{
    if(!window.getSelection()?.isCollapsed)return;
    for(const [id,range] of readingCommentRanges) {
      if(Array.from(range.getClientRects()).some(r=>e.clientX>=r.left && e.clientX<=r.right && e.clientY>=r.top && e.clientY<=r.bottom)) {
        e.preventDefault();e.stopImmediatePropagation();openComments(id);break;
      }
    }
  },true);
  return {
    setPanel:setCommentPanel,toggle:toggleContext,reset:resetComments,openComments,loadComments,startPassageComment,
    // Typing and remote changes while editing.
    textChanged(){if(!$("comments-panel").hidden)editorContents.soon();},
    modeChanged(){
      const editing=!!getContext().editing;
      if(editing===shownEditing)return;
      shownEditing=editing;passageShown=null;
      if(!$("comments-panel").hidden)refreshPanel();
    },
    // A passage waiting for the render that failed is not shown later.
    previewFailed(){passageShown=null;},
    previewRendered(){
      if(!getContext().editing)renderContents();
      paintComments();
      if(passageShown!==null && !getContext().editing)showReadingPassage(passageShown);
      passageShown=null;
      if(discussion)renderComments();
    },
  };
}
