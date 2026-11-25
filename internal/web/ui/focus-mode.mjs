import {$} from './dom.mjs';
const storageKey='gitnotes:focus-mode',settingsKey='gitnotes:focus-settings';
const revealZone=40;

// Focus mode is a per-browser choice that applies while a note is shown: the
// note alone on a page, full screen when the browser allows it. The layout
// hides the sidebar and phone reader bar; CSS hides the remaining chrome. The
// header keeps save state, Edit/Read, Save, exit and the page controls. With a
// mouse on a wide screen it waits below the bottom edge until the pointer,
// keyboard focus or a save that needs attention brings it back. Conflicts,
// notices and drafts are never hidden. Page settings are stored per browser.
// Ctrl/⌘+Shift+. is free in Chromium, Firefox, Safari, the editor and chat.
export function initFocusMode({layout,closePanels,isEditing,focusEditor,openSearch,toggleContext}) {
  const button=$('focus-toggle'),note=$('document'),main=$('main'),header=main.querySelector(':scope > header');
  const mac=/Mac|iPhone|iPad/.test(navigator.userAgentData?.platform || navigator.platform);
  const shortcut=mac?'⌘ Shift .':'Ctrl Shift .';
  const floating=matchMedia('(hover: hover) and (min-width: 721px)');
  const ranges={text:$('focus-text-size'),width:$('focus-page-width'),position:$('focus-page-position'),background:$('focus-background')};
  const toggles={typewriter:$('focus-typewriter'),dim:$('focus-dim')};
  const fullButton=$('focus-fullscreen'),wordCount=$('word-count'),wordsButton=$('focus-words');
  const settings={fullscreen:true,typewriter:false,dim:false};
  for(const [name,input] of Object.entries(ranges))settings[name]=Number(input.value);
  let enabled=false;
  try {
    enabled=localStorage.getItem(storageKey)==='true';
    const saved=JSON.parse(localStorage.getItem(settingsKey)) || {};
    for(const [name,input] of Object.entries(ranges))
      if(Number.isFinite(saved[name]))settings[name]=Math.min(Number(input.max),Math.max(Number(input.min),saved[name]));
    for(const name of ['fullscreen','typewriter','dim'])if(typeof saved[name]==='boolean')settings[name]=saved[name];
  } catch {}
  const available=()=>!note.hidden && !document.body.classList.contains('search-active');
  const active=()=>document.body.classList.contains('focus-mode');
  const autoHide=()=>active() && floating.matches;

  function save(){try {localStorage.setItem(settingsKey,JSON.stringify(settings));} catch {}}
  function applySettings(){
    // CSS reads these only under body.focus-mode, so they never leak into reading.
    const style=document.body.style;
    style.setProperty('--focus-text-scale',settings.text/100);
    style.setProperty('--focus-page-width',settings.width/100);
    style.setProperty('--focus-page-position',settings.position/100);
    style.setProperty('--focus-background',settings.background+'%');
    for(const [name,input] of Object.entries(ranges)){
      input.value=settings[name];
      const value=name==='position'?{0:'Left',50:'Centred',100:'Right'}[settings.position] ?? settings.position+'% from the left':settings[name]+'%';
      input.setAttribute('aria-valuetext',value);
      input.closest('label').querySelector('.focus-value').textContent=name==='position'?value.replace(' from the left',''):value;
    }
    for(const [name,toggle] of Object.entries(toggles)){
      toggle.setAttribute('aria-pressed',String(settings[name]));
      note.toggleAttribute('data-'+name,settings[name]);
    }
  }

  // Full screen needs a click or key press, so a reload keeps the page in the window.
  const fullscreen=()=>document.fullscreenElement===document.documentElement;
  // Screen changes arrive after the request, so a quick toggle can find the
  // last one still on its way: leaving marks an exit this page asked for, and
  // returning takes the screen again once that exit ends.
  let wasFullscreen=false,leaving=false,returning=false;
  fullButton.hidden=!document.fullscreenEnabled;
  function enterFullscreen(){
    if(!document.fullscreenEnabled)return;
    if(leaving){returning=true;return;}
    if(document.fullscreenElement)return;
    // Chromium then gives a short Esc to the page, so menus and panels close
    // before focus mode does; holding Esc still leaves full screen.
    document.documentElement.requestFullscreen({navigationUI:'hide'})
      .then(()=>navigator.keyboard?.lock?.(['Escape'])).catch(()=>{});
  }
  function exitFullscreen(){
    returning=false;
    if(!fullscreen() || leaving)return;
    leaving=true;
    document.exitFullscreen().catch(()=>{leaving=false;});
  }
  document.addEventListener('fullscreenchange',()=>{
    const full=fullscreen();
    // Leaving full screen through the browser (Esc, menu, window control) ends
    // focus mode too; an exit the page asked for already did what it meant.
    if(wasFullscreen && !full && !leaving && active())set(false);
    // Turned off before the screen was taken.
    if(full && !enabled)exitFullscreen();
    if(!full){
      leaving=false;navigator.keyboard?.unlock?.();
      if(returning){returning=false;if(active())enterFullscreen();}
    }
    wasFullscreen=full;
    fullButton.setAttribute('aria-pressed',String(full));
  });
  fullButton.onclick=()=>{
    settings.fullscreen=!fullscreen();save();
    if(settings.fullscreen)enterFullscreen();else exitFullscreen();
  };

  // The toolbar stays while the pointer is on it or keyboard focus is in it.
  let hideTimer=0,pointerInside=false;
  const keyboardInside=()=>!!header.querySelector(':focus-visible');
  function reveal(){clearTimeout(hideTimer);header.setAttribute('data-revealed','');}
  function conceal(delay){
    clearTimeout(hideTimer);
    hideTimer=setTimeout(()=>{if(!pointerInside && !keyboardInside())header.removeAttribute('data-revealed');},delay);
  }
  function pointer(event){
    if(!autoHide())return;
    main.removeAttribute('data-typing');
    pointerInside=header.contains(event.target);
    if(pointerInside || event.clientY>=innerHeight-revealZone)reveal();
    else if(header.hasAttribute('data-revealed'))conceal(500);
  }
  document.addEventListener('pointermove',pointer);
  document.addEventListener('pointerdown',pointer);
  document.documentElement.addEventListener('pointerleave',()=>{pointerInside=false;if(autoHide())conceal(500);});
  header.addEventListener('focusin',event=>{if(autoHide() && event.target.matches(':focus-visible'))reveal();});
  header.addEventListener('focusout',event=>{if(autoHide() && !header.contains(event.relatedTarget))conceal(400);});
  // Writing hides the toolbar and the pointer until the pointer moves again.
  document.addEventListener('keydown',event=>{
    if(!autoHide() || header.contains(event.target) || event.metaKey || event.ctrlKey || event.altKey || ['Shift','Control','Alt','Meta','Escape','Tab'].includes(event.key))return;
    clearTimeout(hideTimer);header.removeAttribute('data-revealed');
    if(note.contains(event.target))main.setAttribute('data-typing','');
  },true);
  // A save that puts the draft at risk keeps the toolbar, and its save state, on screen.
  function attention(){
    header.toggleAttribute('data-attention',active() && ($('save-state').dataset.attention==='true' || !$('draft-conflict').hidden));
  }
  const attentionObserver=new MutationObserver(attention);
  attentionObserver.observe($('save-state'),{attributes:true,attributeFilter:['data-attention']});
  attentionObserver.observe($('draft-conflict'),{attributes:true,attributeFilter:['hidden']});

  // The editor does Typewriter and Dim others itself (frontend/editor-focus.mjs)
  // from the data-typewriter and data-dim attributes, told when they change.
  const tellEditor=()=>document.getElementById('editor')?.dispatchEvent(new Event('fmn-focus-settings'));

  // Words added this session are counted per note from the first count shown in focus mode.
  const sessionStart=new Map();
  function renderWords(){
    const count=Number(wordCount.dataset.words),key=wordCount.dataset.for,known=Boolean(key) && Number.isInteger(count);
    $('focus-word-total').textContent=known?count.toLocaleString()+' words':wordCount.textContent==='Count unavailable'?'Count unavailable':'Counting…';
    if(known && active() && !sessionStart.has(key))sessionStart.set(key,count);
    const added=known && sessionStart.has(key)?count-sessionStart.get(key):0,session=$('focus-word-session');
    session.hidden=!added;
    session.textContent=(added>0?'+':'−')+Math.abs(added).toLocaleString();
    wordsButton.setAttribute('aria-label',$('focus-word-total').textContent+(added?', '+Math.abs(added).toLocaleString()+(added>0?' added':' removed')+' this session':'')+' · open writing tools');
  }
  new MutationObserver(renderWords).observe(wordCount,{attributes:true,attributeFilter:['data-words','data-for']});

  function update() {
    const on=enabled && available(),entering=on && !active();
    if(entering)closePanels();
    const changed=on!==active();
    document.body.classList.toggle('focus-mode',on);
    if(changed)tellEditor();
    layout.setFocusMode(on);
    button.hidden=!available();
    button.textContent=on?'Exit focus':'Focus';
    button.title=on?'Exit focus mode (Esc)':'Focus mode ('+shortcut+')';
    // Show where the toolbar lives, then leave only the page.
    if(entering && floating.matches){reveal();conceal(1600);}
    if(!on){clearTimeout(hideTimer);header.removeAttribute('data-revealed');main.removeAttribute('data-typing');}
    attention();
    renderWords();
    // Closing a panel returns focus to its opener, which focus mode hides.
    const focused=document.activeElement;
    if(on && focused && focused!==document.body && !focused.getClientRects().length)button.focus({preventScroll:true});
  }
  function set(value,gesture=false) {
    enabled=value;
    try {localStorage.setItem(storageKey,String(value));} catch {}
    if(value && gesture && settings.fullscreen && available())enterFullscreen();
    if(!value)exitFullscreen();
    update();
  }
  button.onclick=()=>{
    const on=!active();
    set(on,true);
    if(on && isEditing())focusEditor();
  };
  for(const [name,input] of Object.entries(ranges)){
    input.addEventListener('input',()=>{settings[name]=Number(input.value);applySettings();save();});
    // A dragged slider hands the caret back; keyboard users stay on the control.
    input.addEventListener('change',()=>{if(active() && isEditing() && !input.matches(':focus-visible'))focusEditor();});
  }
  // Toggles keep the caret in the text, as a word processor's controls do.
  for(const control of [...Object.values(toggles),fullButton])control.addEventListener('mousedown',event=>event.preventDefault());
  for(const [name,toggle] of Object.entries(toggles))toggle.onclick=()=>{
    settings[name]=!settings[name];applySettings();save();
    tellEditor();
  };
  $('focus-goto').title='Go to another note ('+(mac?'⌘ K':'Ctrl K')+')';
  $('focus-goto').onclick=()=>openSearch();
  $('focus-context').onclick=event=>toggleContext(event.currentTarget);
  // Window listeners run after document handlers, which close their own menus
  // and panels on Escape first and mark the event handled.
  window.addEventListener('keydown',event=>{
    if(event.defaultPrevented || event.isComposing)return;
    const toggle=(event.ctrlKey || event.metaKey) && event.shiftKey && !event.altKey && event.code==='Period';
    const leave=event.key==='Escape' && !event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey && active();
    if(!(toggle ? available() : leave) || document.querySelector('dialog[open], :popover-open'))return;
    event.preventDefault();set(toggle && !active(),true);
  });
  floating.addEventListener('change',()=>{header.removeAttribute('data-revealed');main.removeAttribute('data-typing');});
  const observer=new MutationObserver(update);
  observer.observe(document.body,{attributes:true,attributeFilter:['class']});
  observer.observe(note,{attributes:true,attributeFilter:['hidden']});
  applySettings();
  update();
}
