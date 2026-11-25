import {openDialog} from './overlay.mjs';
import {coverURL} from './browse.mjs';
import {notebookPath} from './request.mjs';
import {$,busy} from './dom.mjs';
import {confirmAction} from './feedback.mjs';
import {pendingContainer} from './containers.mjs';

// The open note's properties: the Properties dialog (display title, kind,
// status, tags, cover, collection and Trash), the metadata bar and cover above
// the note, and collection changes for notes and folders. The caller owns the
// note, saves (`beginWrite`/`endWrite`), its retained drafts (`draftPaths`) and
// replacing the open note with a write's saved version (`writeNote`).
export function initProperties({api,post,notice,getContext,getContent,currentNotebook,dirty,canEdit,draftPaths,beginWrite,endWrite,writeNote,renderProtection,openNote,openFolder,openSearch,invalidateSearch,refresh,moveToTrash}) {
  const error=message=>{$("properties-error").textContent=message;$("properties-error").hidden=false;};
  function renderContainerActions() {
   const {current}=getContext(),m=current?.metadata || {};
   for(const [value,id] of [["","notes"],["draft","draft"],["archive","archive"]]){const button=$("note-container-"+id);button.hidden=(m.container || "")===value && !m.containerError;button.disabled=!current?.editable || dirty() || !!m.containerError;}
  }
  let propertiesInitial;
  function propertiesValues(){return JSON.stringify(["title","kind","status","tags","cover"].map(key=>$("property-"+key).value));}
  const hasChanges=()=>propertiesValues()!==propertiesInitial;
  $("properties-open").onclick = () => {
    const {current,project,saving}=getContext();
    if (!current?.revision || saving) return;
    $("properties-path").textContent = current.path;
    const m = current.metadata || {};
    for (const key of ["title","kind","status"]) $("property-" + key).value = m[key] || "";
    $("property-tags").value = (m.tags || []).join("\n");
    const problem = current.metadataError || (!current.editable ? "This note is read-only." : dirty() ? "Save or reconcile your draft before editing properties." : "");
    $("properties-error").textContent = problem;$("properties-error").hidden = !problem;
    for (const key of ["title","kind","status","tags"]) $("property-" + key).disabled = !!problem;
    $("properties-save").disabled = !!problem;
    $("property-cover").replaceChildren(new Option("No cover",""));
    if(m.cover)$("property-cover").add(new Option(m.cover,m.cover));
    $("property-cover").value=m.cover || "";
    $("property-cover").disabled=!!problem;$("cover-upload-open").disabled=!!problem;
    previewCover();
    propertiesInitial=propertiesValues();renderProtection();
    renderContainerActions();
    openDialog($("properties-dialog"));
    const selectedNote=current.path, selectedProject=project.id;
    api(notebookPath(project.id)+"/api/notes").then(entries=>{
      if(getContext().current?.path!==selectedNote || getContext().project.id!==selectedProject || !$("properties-dialog").open)return;
      const selected=$("property-cover").value;
      const paths=entries.map(entry=>entry.path).filter(path=>coverURL(selectedProject,path));
      if(selected && !paths.includes(selected))paths.push(selected);
      $("property-cover").replaceChildren(new Option("No cover",""),...paths.sort().map(path=>new Option(path,path)));
      $("property-cover").value=selected;
    }).catch(failure=>error(failure.message));
  };
  $("properties-form").onsubmit = event => {
    event.preventDefault();
    const {current,project,saving}=getContext();
    if (!current?.editable || dirty() || saving) return;
    const path = current.path;
    busy($("properties-form"), async () => {
      beginWrite();
      try {await post(notebookPath(project.id)+"/api/metadata", {path,revision:current.revision,metadata:{cover:$("property-cover").value,title:$("property-title").value,kind:$("property-kind").value,status:$("property-status").value,tags:$("property-tags").value.split("\n")}});}
      finally {endWrite();}
      $("properties-dialog").close();
      await openNote(path);await refresh();
    }, {controls:$("properties-save"), report:$("properties-error")});
  };

  function previewCover() {
   const url=coverURL(getContext().project.id,$("property-cover").value);
   $("cover-preview").hidden=!url;if(url)$("cover-preview").src=url;else $("cover-preview").removeAttribute("src");
  }
  $("cover-preview").onerror=()=>{$("cover-preview").hidden=true;};
  $("property-cover").onchange=previewCover;
  $("cover-upload-open").onclick=()=>$("cover-upload").click();
  $("cover-upload").onchange=()=>{
   const file=$("cover-upload").files[0],{current,project,saving}=getContext();
   if(!file || saving || !current?.editable || dirty())return;
   const extension=file.name.split('.').pop().toLowerCase(),path='assets/covers/'+crypto.randomUUID()+'.'+extension;
   if(!coverURL(project.id,path)) {error('Choose a PNG, JPEG, WebP, GIF, or AVIF image.');return;}
   const body=new FormData();body.append('file',file);body.append('path',path);body.append('revision',current.revision);
   return busy($("cover-upload"), async () => {
    beginWrite();
    try {
     const result=await api(notebookPath(project.id)+'/api/upload',{method:'POST',body});
     $("property-cover").add(new Option(result.path,result.path));$("property-cover").value=result.path;previewCover();
     $("properties-error").textContent='';
    } finally {endWrite();}
   }, {controls:[$("properties-save"),$("cover-upload-open")], report:$("properties-error")}).finally(()=>{$("cover-upload").value='';});
  };

  for(const [value,id] of [['','notes'],['draft','draft'],['archive','archive']])$('note-container-'+id).onclick=()=>{
   const {current}=getContext();
   if(!current)return;
   classifyNote(current.path,value,current.revision).catch(failure=>error(failure.message));
  };
  $("note-trash").onclick = () => {
   const {current,saving}=getContext();
   if (!current?.revision || saving) return;
   if(hasChanges()){error("Save your property changes before moving this note to Trash.");return;}
   return busy($("note-trash"), () => moveToTrash(current.path,error), {controls:[...$("properties-form").elements]}).finally(renderProtection);
  };

  // The cover and metadata bar above the open note (or a revision of it).
  function showCover(metadata,revision) {
    const {project}=getContext(),url=project && coverURL(project.id,metadata?.cover,revision);
    $("note-cover").hidden=!url;
    if(url)$("note-cover").src=url;else $("note-cover").removeAttribute("src");
    $("note-cover").onerror=()=>{$("note-cover").hidden=true;};
  }
  function render() {
    const {current}=getContext();
    $("properties-open").disabled = !current?.revision;
    showCover(current?.metadata,current?.revision);
    $("metadata-bar").replaceChildren();
    const metadata = current?.metadata;
    $("metadata-bar").hidden = !metadata || !(metadata.status || metadata.tags?.length);
    renderBadge();
    if (!metadata) return;
    if (metadata.status) {
      const label = document.createElement("span");label.className = "property-chip property-status";label.textContent = metadata.status;$("metadata-bar").append(label);
    }
    for (const tag of metadata.tags || []) {
      const button = document.createElement("button");button.className = "property-chip quiet";button.textContent = "#" + tag;
      button.title = "Filter notes tagged " + tag;
      button.onclick = async () => {
        if (![...$("tag-filter").options].some(option => option.value === tag.toLowerCase())) $("tag-filter").add(new Option(tag,tag.toLowerCase()));
        $("tag-filter").value = tag.toLowerCase();
        await openSearch();
      };
      $("metadata-bar").append(button);
    }
  }
  // The collection badge says where the note is, and where it moves on Save.
  function renderBadge() {
   $('container-badge')?.remove();
   const {current,original}=getContext();
   if(!current)return;
   const invalid=current.metadata?.containerError,saved=invalid?'invalid':current.metadata?.container || '';
   const metadataCurrent=current.metadataVersion===undefined || current.metadataVersion===current.version;
   const content=getContent(),pending=metadataCurrent && content===original?saved:pendingContainer(content);
   if(!saved && !invalid && pending===saved)return;
   const badge=document.createElement('span');badge.id='container-badge';badge.className='property-chip';
   const label=value=>({draft:'Shared draft',archive:'Archive',invalid:'Invalid collection'})[value] || 'Notes';
   badge.textContent=invalid?'Invalid collection':label(saved);
   if(pending!==saved)badge.textContent+=' → '+label(pending)+' · pending Save';
   if(invalid)badge.title=invalid;
   $('metadata-bar').prepend(badge);$('metadata-bar').hidden=false;
  }

  const notebookDrafts=()=>{const {user,project}=getContext();return draftPaths(user.id+'/'+project.id+'/');};
  // A folder move classifies every note under it in one request bound to the
  // revision the menu loaded; the server rejects it if any of those notes changed.
  async function classifyFolder(folder,container,revision,count) {
   const {saving,current}=getContext();
   if(saving || !canEdit())return;
   const prefix=folder+'/',destination={'':'Notes',draft:'Drafts',archive:'Archives'}[container];
   if(current?.path.startsWith(prefix) && dirty() || notebookDrafts().some(path=>path.startsWith(prefix)))throw Error('Save or reconcile the private drafts in this folder before moving it.');
   if(!await confirmAction(`Move ${count} ${count===1?'note':'notes'} in “${folder}” to ${destination}?`))return;
   const result=await post(currentNotebook()+'/api/folder-container',{folder,container,revision});
   invalidateSearch();
   const browsing=getContext().folder;
   if(browsing!==null)await openFolder(browsing,false);
   await refresh();
   notice(`Moved ${result.changed} ${result.changed===1?'note':'notes'} to ${destination}.`,true);
  }
  async function classifyNote(path,container,revision) {
   const {saving,current}=getContext();
   if(saving || !canEdit())return;
   if(current?.path===path && dirty() || notebookDrafts().includes(path))throw Error('Save or reconcile this note’s private draft before changing its collection.');
   if($('properties-dialog').open && hasChanges())throw Error('Save your property changes before changing this note’s collection.');
   await writeNote(path,async install=>{
    install(await post(currentNotebook()+'/api/container',{path,container,revision}));
    await refresh({invalidate:true});
   });
   $('properties-dialog').close();
   const browsing=getContext().folder;
   if(browsing!==null)await openFolder(browsing,false);
  }
  return {render,renderBadge,showCover,renderContainerActions,hasChanges,classifyNote,classifyFolder};
}
