import {icon} from './icons.mjs';
import {requestSequence} from './state.mjs';
import {documentURL} from './documents.mjs';
import {navigationURL} from './navigation.mjs';
import {historyResultDetail} from './knowledge.mjs';
import {noteTitle,destinationFolders} from './editor.mjs';
import {notebookPath} from './request.mjs';
import {$,routeLink} from './dom.mjs';

// Wrap query terms in <mark> using text nodes only; result text is untrusted.
function highlighted(parent, value, query) {
  const text=String(value??'');
  const terms=(query||'').trim().split(/\s+/).filter(term=>term.length>1).map(term=>term.replace(/[.*+?^${}()|[\]\\]/g,'\\$&'));
  if(!terms.length){parent.append(text);return;}
  // Mark the whole word that contains a term, not just the matched prefix.
  for(const [index,part] of text.split(new RegExp('([\\p{L}\\p{N}_]*(?:'+terms.join('|')+')[\\p{L}\\p{N}_]*)','giu')).entries()) {
    if(!part)continue;
    if(index%2){const mark=document.createElement('mark');mark.textContent=part;parent.append(mark);}
    else parent.append(part);
  }
}
const folderOf=path=>path.includes('/')?path.slice(0,path.lastIndexOf('/')):'';
function resultRow(title, detail, excerpt, query='') {
  const item=document.createElement('li'),row=document.createElement('div'),link=document.createElement('a');
  row.className='note-row';
  highlighted(link,title,query);
  // Excerpts begin with the note's heading, which the row already shows as its title.
  const heading=String(title??'').trim();
  if(heading && typeof excerpt==='string' && excerpt.startsWith(heading+' '))excerpt=excerpt.slice(heading.length).trimStart();
  for(const [text,className] of [[detail,'note-filename'],[excerpt,'search-excerpt']]) {
    if(!text)continue;
    const description=document.createElement('small');
    description.className=className;
    if(className==='search-excerpt')highlighted(description,text,query);else description.textContent=text;
    link.append(description);
  }
  row.append(link);item.append(row);
  return {item,row,link};
}

// Search owns its scope, pending requests and rendered result callbacks. The
// application retains navigation/editor state and supplies those operations.
export function initSearch({
  api,post,getContext,retrievalSettings,notice,
  onOpen,onClose,onVisibilityChange,onStateChange,renderCollections,
  openNote,openCitation,openTrash,openSource,setupFileActions,canEdit,canCreate,addSource,
  sourcesOn=()=>true,repositoriesOn=()=>true,judgmentsOn=()=>true,
}) {
  let searchOpen=false,searchCollection='',searchScroll=0;const requests=requestSequence();
  let searchController;
  function invalidateSearchResults() {
    $("search-results").replaceChildren();
    $("search-view").hidden=true;
    document.body.classList.remove("search-active");
  }
  function cancelSearch() {
    $("search-results").replaceChildren();
    requests.next();
    searchController?.abort();
    clearTimeout(searchTimer);
  }
  let navigationEntries = null, navigationProject = null;
  let noteMeaningAvailable=false;
  const scopeInputs=()=>[...$('search-projects').querySelectorAll('input:checked')];
  function renderNotebookScope(selected){
   const {project,projects}=getContext();
   $('search-projects').replaceChildren(...projects.map(notebook=>{const label=document.createElement('label'),input=document.createElement('input');input.type='checkbox';input.value=notebook.id;input.checked=selected.size?selected.has(notebook.id):notebook.id===project.id;label.append(input,document.createTextNode(notebook.name));return label;}));
  }
  function syncMeaningControl(){const source=$("search-corpus").value!=="notes"||scopeInputs().some(o=>o.value!==getContext().project?.id);$("search-meaning").disabled=!source&&!noteMeaningAvailable;if($("search-meaning").disabled&&$("search-meaning").checked)$("search-all").checked=true;}
  // rerank asks for one judgment ranking of this search. Only an explicit Enter
  // sends it; typing stays local, and the current results stay visible meanwhile.
  async function refreshList(query = "", reuseNavigation = false, {rerank = false} = {}) {
    const {project,user,collection}=getContext();
    if(searchOpen)onStateChange?.(searchSnapshot());
    searchController?.abort();
    if(!rerank)$("search-results").replaceChildren();
    searchController = new AbortController();
    const signal = searchController.signal;
    const projectID=project?.id,accountID=user?.id;
    if(!projectID){invalidateSearchResults();return;}
    const ownsResults=requests.begin(()=>getContext().project?.id===projectID && getContext().user?.id===accountID);
    const searching = !!query.trim();
    if(!searching && project.kind==='repository'){
      navigationEntries=null;navigationProject=projectID;
      renderCollections();
      $('search-status').textContent=searchOpen?'Search repository files':'';
      return;
    }
    if(searching && ($("search-corpus").value!=="notes" || scopeInputs().some(o=>o.value!==projectID))) {
      renderSearchOptions();
      $("search-status").textContent="Searching…";
      const selected=scopeInputs().map(option=>option.value);
      const mode=document.querySelector('input[name="search-mode"]:checked').value;
      try {
        const result=await post("/api/retrieval",{projects:selected.length?selected:[projectID],corpora:$("search-corpus").value==="both"?["notes","repository"]:[$("search-corpus").value],query:query.trim(),mode:mode==="all"?"auto":mode,history:$("search-scope").checked,pathPrefix:$("folder-filter").value,...retrievalSettings.preferences()},{signal});
        if(!ownsResults())return;
        const coverage=Object.entries(result.sources.coverage||{}).map(([id,c])=>`${id}: ${c.indexed?.slice(0,12)||"not indexed"} · ${c.embedded||0}/${c.chunks||0} embedded${c.message?" · "+c.message:""}`);
        $("search-status").textContent=`${result.results.length} matches${result.truncated?" · more matches available":""}. `+coverage.join("; ");
        const list=document.createElement("ul");list.className="note-tree";
        for(const item of result.results) {
          const r=item.source||item.note;
          const detail=item.source
            ? `${item.project} · ${r.path}:${r.startLine}–${r.endLine} · ${r.revision.slice(0,12)}`
            : `${item.project} · ${r.path}`;
          const {item:element,link}=resultRow(r.symbol||r.heading||r.path,detail,item.excerpt??r.excerpt??r.text,query);
          link.href=item.source?documentURL({project:item.project,origin:"repository",source:r.source,revision:r.revision,path:r.path,startLine:r.startLine,endLine:r.endLine}):navigationURL({project:item.project,note:r.path,revision:r.revision});
          if(item.source)routeLink(link,()=>{
            if(ownsResults())openSource(r.source,r.revision,r.path,{project:item.project,startLine:r.startLine,endLine:r.endLine}).catch(error=>notice(error.message));
          });
          list.append(element);
        }
        $("search-results").replaceChildren(list);
        if(!result.results.length){
          const empty=document.createElement("p");
          empty.textContent="No matches in the available source coverage.";
          $("search-results").append(empty);
        }
      }catch(error){if(ownsResults()&&error.name!=="AbortError")$("search-status").textContent=error.message;}
      return;
    }
    const preferences=retrievalSettings.preferences();
    const contentSelection={...preferences,...(searchCollection?{collection:searchCollection}:{})};
    const mode = document.querySelector('input[name="search-mode"]:checked').value;
    const scope = $("search-scope").checked ? "history" : "current";
    const includeTrash = $("include-trash").checked;
    renderSearchOptions();
    const ranking = rerank && judgmentsOn() && searching && mode !== "text" && scope === "current";
    $("search-status").textContent = ranking ? "Ranking with judgments…" : searching ? "Searching…" : "";
    const tag = $("tag-filter").value, statusFilter = $("status-filter").value, path = $("folder-filter").value;
    const filteredQuery = new URLSearchParams({q:query, tag, status:statusFilter, path,...contentSelection});
    let allEntries, matches, collectionEntries, collectionCounts;
    try {
      [allEntries, matches, collectionEntries, collectionCounts] = await Promise.all([
        reuseNavigation && navigationProject === project.id && navigationEntries ? Promise.resolve(navigationEntries) : api(notebookPath(projectID)+"/api/notes", {signal}),
        searching ? post(notebookPath(projectID)+"/api/knowledge/hybrid-search", {text:query.trim(), mode, scope, includeTrash, tag, status:statusFilter, path, limit:50,...contentSelection,...(ranking?{rerank:true}:{})}, {signal}) : includeTrash || tag || statusFilter || path ? api(notebookPath(projectID)+"/api/notes?" + filteredQuery + "&includeTrash=" + includeTrash, {signal}) : Promise.resolve(null),
        searching ? Promise.resolve(null) : api(notebookPath(projectID)+"/api/notes?collection="+collection,{signal}),
        api(notebookPath(projectID)+"/api/collections",{signal}),
      ]);
    } catch (error) {
      if (!ownsResults() || error.name === "AbortError") return;
      $("search-status").textContent = error.message;
      $("search-results").replaceChildren();
      return error;
    }
    if (!ownsResults()) return;
    navigationEntries=allEntries;navigationProject=projectID;
    renderCollections(collectionCounts);
    const entries = searching ? matches.matches : matches ? matches : collectionEntries;
    // Automatic mode falling back to text is routine, not news; explicit modes keep their wording.
    const count=`${entries.length}${entries.length===50 ? "+" : ""}`;
    const routine=mode==="all" && /^Semantic search is off\.( Showing text matches\.)?$/.test(matches?.message||"");
    $("search-status").textContent = searching
      ? [routine ? "" : matches.message, mode==="all" ? `${count} ${entries.length === 1 ? "result" : "results"}` : `${count} ${matches.mode==="meaning" ? "meaning" : "text"} ${entries.length === 1 ? "match" : "matches"}`, matches.reranked ? "Ranked by judgments" : matches.rerankAvailable && !ranking && mode !== "text" && entries.length > 1 ? "Press Enter to rank with judgments" : ""].filter(Boolean).join(" · ")
      : scope === "history" ? "Enter a search to include previous versions." : "";
    const historicalFilters=matches?.filters;
    updateFilterOptions(historicalFilters ? [...historicalFilters.paths.map(path=>({path})),{path:"",metadata:{tags:historicalFilters.tags}},...historicalFilters.statuses.map(status=>({path:"",metadata:{status}}))] : (matches?.matches || matches || collectionEntries || allEntries));
    const activeFilters = [tag, statusFilter, path].filter(Boolean).length;
    $("clear-filters").disabled = !activeFilters;
    const filtered = !!(query.trim() || tag || statusFilter || path);
    const resultsVisible=!!(searchOpen || searching || tag || statusFilter || path || includeTrash);
    function render(nodes) {
      const list = document.createElement("ul");
      list.className = "note-tree";
      for (const node of nodes) {
        const {item,row,link}=resultRow(node.name,node.detail,node.excerpt,searching?query:"");
        link.draggable = false;
          link.title = node.path;
          link.href =
            navigationURL({project:projectID,collection,note:node.path,...(node.revision ? {revision:node.revision}: {})});
          if(node.trashId)link.href="/?project="+projectID+"&trash="+encodeURIComponent(node.trashId);
          link.dataset.note = node.path;
          if (node.revision) link.dataset.revision = node.revision;
          routeLink(link, () => {
            if (node.trashId) openTrash(node.trashId);
            else if (node.revision) openCitation(node.path,node.revision,node.heading || "",node.hit).catch(error=>notice(error.message));
            else openNote(node.path);
          }, {active:()=>ownsResults() && link.isConnected});
          if (!node.revision && !node.trashId) setupFileActions(row, node, ownsResults);
          list.append(item);
      }
      return list;
    }
    const tree = searching ? entries.map(hit => ({trashId:hit.trashId, kind:"note", path:hit.path, name:hit.title, detail:[hit.trashId ? "Trash" : "", scope==="history" ? historyResultDetail(hit) : folderOf(hit.path)].filter(Boolean).join(" · "), excerpt:hit.excerpt ?? hit.text, revision:scope==="history" ? hit.revision : "", heading:hit.heading, hit})) : entries.map(entry=>({kind:"note",path:entry.path,name:noteTitle(entry),detail:folderOf(entry.path),excerpt:entry.excerpt,trashId:entry.trashId})).sort((a,b)=>a.name.localeCompare(b.name,undefined,{numeric:true,sensitivity:"base"}));
    if(!resultsVisible)$("search-results").replaceChildren();
    const resultHost=$("search-results");
    if(resultsVisible)resultHost.replaceChildren(render(tree));
    $("search-view").hidden=!searchOpen;
    document.body.classList.toggle("search-active",searchOpen);
    onVisibilityChange();
    if(!$("search-status").textContent)$("search-status").textContent=entries.length+(entries.length===1?" note":" notes");
    if (resultsVisible && !tree.length) {
      const empty = document.createElement("p");
      empty.className = "small";
      empty.textContent = searching && matches.history && (matches.history.completedRevision!==matches.history.revision || ["limited","unavailable"].includes(matches.history.state)) ? "No matches in the scanned versions" : filtered ? "No matching notes" : "No notes yet";
      resultHost.append(empty);
      if (searching && scope === "current") {
        const previous = document.createElement("button");
        previous.className = "quiet";
        previous.textContent = "Search previous versions";
        previous.onclick = () => {
          if(!ownsResults())return;
          $("search-scope").checked = true;
          $("search").focus();
          $("search-scope").onchange();
        };
        resultHost.append(previous);
      } else if (!filtered && canEdit() && sourcesOn()) {
        const source = document.createElement("button");
        source.id = "empty-source";
        source.className = "quiet";
        source.textContent = "Add source";
        source.disabled = !canCreate();
        source.onclick = () => addSource();
        resultHost.append(source);
      }
    }
    if (searching) $("search-results").scrollTop = 0;
  }
  function searchSnapshot(){
   const {user,project}=getContext();
   const values={};for(const id of ['search','folder-filter','tag-filter','status-filter','search-archives','search-drafts'])values[id]=$(id).value;
   return {values,notebooks:scopeInputs().map(input=>input.value),corpus:$("search-corpus").value,account:user?.id,project:project?.id,collection:searchCollection,history:$("search-scope").checked,trash:$("include-trash").checked,mode:document.querySelector('input[name="search-mode"]:checked').value};
  }
  function restoreSearch(snapshot){
   for(const [id,value] of Object.entries(snapshot.values)){
    const control=$(id);if(control.tagName==='SELECT' && value && ![...control.options].some(option=>option.value===value))control.add(new Option(value,value));control.value=value;
   }
   renderNotebookScope(new Set(snapshot.notebooks||[]));if(snapshot.corpus)$("search-corpus").value=snapshot.corpus;searchCollection=snapshot.collection;$("search-scope").checked=snapshot.history;$("include-trash").checked=snapshot.trash;
   document.querySelector('input[name="search-mode"][value="'+snapshot.mode+'"]').checked=true;
  }
  async function openSearch(restoreScroll = 0) {
    const {project,projects,current,inboxVisible}=getContext();
    if(!project)return;
    if(!searchOpen)searchScroll=window.scrollY;
    const notebook=document.createElement('span'),divider=document.createElement('span');
    notebook.className='search-notebook';notebook.textContent=project.name;divider.textContent='/';divider.setAttribute('aria-hidden','true');
    $("search-results-title").replaceChildren(notebook,divider,'Search');
    $("search-close").setAttribute("aria-label",current?"Back to note":inboxVisible?"Back to Inbox":"Back to library");
    const selected=new Set(scopeInputs().map(o=>o.value));
    renderNotebookScope(selected);
    searchOpen=true;onOpen();
    $("search-view").hidden=false;document.body.classList.add("search-active");
    onVisibilityChange();$("search").focus({preventScroll:true});window.scrollTo(0,0);
    const request=requests.current()+1;
    await refreshList($("search").value,true).catch(error=>notice(error.message));
    if(searchOpen && requests.owns(request) && typeof restoreScroll==="number")window.scrollTo(0,restoreScroll);
  }
  $("library-tags").onclick=()=>{$("search").value="";cancelSearch();openSearch();showSearchOptions(true);$("tag-filter").focus();};
  $("search-corpus").onchange=()=>{syncMeaningControl();$("search-projects-label").hidden=false;$("search").placeholder=$("search-corpus").value==="notes"?"Search your notes…":"Search source and documentation…";refreshList($("search").value,true);};
  $("search-projects").onchange=()=>{if(!scopeInputs().length){const current=[...$("search-projects").querySelectorAll("input")].find(input=>input.value===getContext().project?.id);if(current)current.checked=true;}syncMeaningControl();refreshList($("search").value,true);};
  $("search-open").onclick=openSearch;
  $("search-close").onclick=()=>{hide();window.scrollTo(0,searchScroll);onClose();};
  document.addEventListener("keydown",event=>{if((event.metaKey || event.ctrlKey) && event.key.toLowerCase()==="k" && !document.querySelector("dialog[open]")){event.preventDefault();openSearch();}});

  let searchTimer;
  function showSearchOptions(open) {
    $("note-filters").hidden = !open;
    $("search-options-toggle").setAttribute("aria-expanded", String(open));
  }
  $("search-options-toggle").onclick = () => showSearchOptions($("note-filters").hidden);
  $("note-filters").addEventListener("keydown", event => {
    if (event.key === "Escape") {
      showSearchOptions(false);
      $("search-options-toggle").focus();
      event.preventDefault();
    }
  });
  function renderSearchOptions() {
    const chips = [];
    function chip(label, reset) {
      const button = document.createElement("button");
      button.className = "quiet search-chip";
      button.textContent = label;button.append(icon("close"));
      button.setAttribute("aria-label", "Remove " + label);
      button.onclick = () => {
        reset();
        $("search-options-toggle").focus();
        cancelSearch();
        refreshList($("search").value).catch(error => notice(error.message));
      };
      chips.push(button);
    }
    retrievalSettings.chips(chip);
    if(searchCollection && ($("search").value.trim() || $("folder-filter").value || $("tag-filter").value || $("status-filter").value || $("include-trash").checked))chip("Collection: "+(searchCollection==="archives"?"Archives":"Drafts"),()=>{searchCollection=""});
    if ($("include-trash").checked) chip("Trash", () => {$("include-trash").checked = false;});
    if ($("search-scope").checked) chip("Previous versions", () => {$("search-scope").checked = false;});
    const mode = document.querySelector('input[name="search-mode"]:checked');
    if (mode.value !== "all") chip(mode.value === "text" ? "Text" : "Meaning", () => {$("search-all").checked = true;});
    for (const [id, label] of [["folder-filter","Folder"],["tag-filter","Tag"],["status-filter","Status"]]) {
      const select = $(id);
      if (select.value) chip(label + ": " + select.selectedOptions[0].text, () => {select.value = "";});
    }
    $("active-search-options").replaceChildren(...chips);
    $("active-search-options").hidden = !chips.length;
    $("clear-filters").disabled = !["folder-filter","tag-filter","status-filter"].some(id => $(id).value);
  }
  $("note-filters").addEventListener("change",()=>{searchOpen=true;},true);
  $("active-search-options").addEventListener("click",()=>{searchOpen=true;},true);
  $("search").onkeydown = event => {
    if (event.key !== "Enter" || event.isComposing || !$("search").value.trim()) return;
    event.preventDefault();
    clearTimeout(searchTimer);
    searchOpen=true;
    refreshList($("search").value,true,{rerank:true}).catch((e) => notice(e.message));
  };
  $("search").oninput = () => {
    searchOpen=true;
    cancelSearch();
    searchTimer = setTimeout(
      () => refreshList($("search").value,true).catch((e) => notice(e.message)),
      300,
    );
  };
  for (const input of document.querySelectorAll('input[name="search-mode"]')) input.onchange = () => {
    cancelSearch();
    refreshList($("search").value).catch(error => notice(error.message));
  };
  $("include-trash").onchange = () => {cancelSearch();refreshList($("search").value).catch(error=>notice(error.message));};
  $("search-scope").onchange = () => {cancelSearch();refreshList($("search").value).catch(error=>notice(error.message));};
  function updateFilterOptions(entries) {
    const folderSelect = $("folder-filter"), selectedFolder = folderSelect.value;
    const folders = destinationFolders(entries).filter(Boolean).map(path => path + "/");
    if (selectedFolder && !folders.includes(selectedFolder)) folders.push(selectedFolder);
    folderSelect.replaceChildren(new Option("All folders", ""), ...folders.map(path => new Option(path.slice(0,-1), path)));
    folderSelect.value = selectedFolder;
    for (const [id, key, label] of [["tag-filter","tags","All tags"],["status-filter","status","All statuses"]]) {
      const select = $(id), selected = select.value, values = new Map();
      for (const entry of entries) {
        const valuesForNote = key === "tags" ? entry.metadata?.tags || [] : [entry.metadata?.status];
        for (const value of valuesForNote) if (value) values.set(value.toLowerCase(), value);
      }
      if (selected && !values.has(selected.toLowerCase())) values.set(selected.toLowerCase(),selected);
      select.replaceChildren(new Option(label,""), ...[...values].sort((a,b) => a[0].localeCompare(b[0])).map(([key,value]) => new Option(value,key)));
      select.value = selected.toLowerCase();
    }
  }
  for (const id of ["folder-filter","tag-filter","status-filter"]) $(id).onchange = () => {cancelSearch(); refreshList($("search").value).catch(error => notice(error.message));};
  $("clear-filters").onclick = () => {
    $("tag-filter").value = ""; $("status-filter").value = "";
    $("folder-filter").value = "";
    cancelSearch();
    refreshList($("search").value).catch(error => notice(error.message));
  };

  function hide(){
    searchOpen=false;
    invalidateSearchResults();
    onVisibilityChange();
  }
  function setCollection(collection){
    searchCollection=collection==='notes'?'':collection;
    for(const id of ['search','folder-filter','tag-filter','status-filter'])$(id).value='';
  }
  function reset(collection){
    cancelSearch();hide();navigationEntries=null;navigationProject=null;
    $('search-all').checked=true;$('search-scope').checked=false;$('include-trash').checked=false;
    showSearchOptions(false);$('search-status').textContent='';
    // Repository source is searchable only while the repositories module runs.
    for(const option of $('search-corpus').options)if(option.value!=='notes'&&option.hidden===repositoriesOn())option.hidden=option.disabled=!repositoriesOn();
    $('search-corpus').value=getContext().project?.kind==='repository'&&repositoriesOn()?'repository':'notes';$('search-projects').replaceChildren();
    $('search-projects-label').hidden=false;$('search').placeholder='Search your notes…';
    setCollection(collection);renderSearchOptions();
  }
  function setMeaningAvailable(available){
    const wasMeaning=$('search-meaning').checked;
    noteMeaningAvailable=available;syncMeaningControl();
    if(wasMeaning && !$('search-meaning').checked)refreshList($('search').value).catch(error=>notice(error.message));
  }
  return {refresh:refreshList,cancel:cancelSearch,open:openSearch,hide,reset,setCollection,setMeaningAvailable,snapshot:searchSnapshot,restore:restoreSearch,
    restoreURL(query){const snapshot=searchFromURL(query,getContext());if(!snapshot)return false;restoreSearch(snapshot);return true;},
    invalidateCache(){navigationEntries=null;},get isOpen(){return searchOpen;}};
}

const searchValues=['search','folder-filter','tag-filter','status-filter','search-archives','search-drafts'];
const searchKeys=['search','q','search-notebook','search-corpus','search-mode','search-history','search-trash','search-collection',...searchValues.filter(id=>id!=='search').map(id=>'search-'+id)];
export function searchToURL(base,snapshot){
 const query=new URLSearchParams(base);for(const key of searchKeys)query.delete(key);
 if(!snapshot)return query;
 for(const key of ['page','category','account'])query.delete(key);
 query.set('search','1');query.set('q',snapshot.values.search);
 for(const id of searchValues.filter(id=>id!=='search'))if(snapshot.values[id])query.set('search-'+id,snapshot.values[id]);
 for(const notebook of snapshot.notebooks)query.append('search-notebook',notebook);
 query.set('search-corpus',snapshot.corpus);query.set('search-mode',snapshot.mode);
 if(snapshot.history)query.set('search-history','1');if(snapshot.trash)query.set('search-trash','1');if(snapshot.collection)query.set('search-collection',snapshot.collection);
 return query;
}
export function searchFromURL(query,{user,project,projects}){
 if(query.get('search')!=='1' || !user || !project)return null;
 const notebooks=query.getAll('search-notebook');
 if(notebooks.some(id=>!projects.some(book=>book.id===id)))throw Error('A search notebook is unavailable or access denied.');
 const choice=(key,choices,fallback)=>choices.includes(query.get(key))?query.get(key):fallback;
 const values={search:query.get('q')||''};
 for(const id of searchValues.filter(id=>id!=='search'))values[id]=query.get('search-'+id)||'';
 for(const id of ['search-archives','search-drafts'])if(!['','include','lower','exclude'].includes(values[id]))values[id]='';
 return {values,notebooks:notebooks.length?notebooks:[project.id],corpus:choice('search-corpus',['notes','repository','both'],'notes'),mode:choice('search-mode',['all','text','meaning'],'all'),history:query.get('search-history')==='1',trash:query.get('search-trash')==='1',collection:choice('search-collection',['drafts','archives'],''),account:user.id,project:project.id};
}
