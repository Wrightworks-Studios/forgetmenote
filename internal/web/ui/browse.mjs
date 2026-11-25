import {icon} from './icons.mjs';
import {navigationURL} from './navigation.mjs';
import {assetURL} from './request.mjs';
import {routeLink} from './dom.mjs';

export function coverURL(project, cover, revision = '') {
  if (typeof cover !== 'string' || !cover.startsWith('assets/') || /[\\\x00-\x1f?#%]/.test(cover) || cover.split('/').some(part => !part || part.startsWith('.')) || !/\.(png|jpe?g|webp|gif|avif)$/i.test(cover)) return null;
  return assetURL(project, cover, revision);
}

export function initFolderBrowser({root, openFolder, openNote, setupActions}) {
  let snapshot, project, user;
  const $ = id => root.querySelector('#' + id);
  function preferenceKey() {return 'forgetmenote:browse:' + user + ':' + project.id;}
  function setView(view) {
    $('folder-items').dataset.view = view;
    for (const button of root.querySelectorAll('[data-browse-view]')) button.setAttribute('aria-pressed',String(button.dataset.browseView === view));
  }
  for (const button of root.querySelectorAll('[data-browse-view]')) button.onclick = () => {
    setView(button.dataset.browseView);
    try {localStorage.setItem(preferenceKey(),button.dataset.browseView);} catch {}
  };
  function link(label,path,folder,collection=snapshot.collection || 'notes') {
    const a=document.createElement('a');
    a.textContent=label;
    a.href=navigationURL({project:project.id,collection,...(folder?{folder:path}:{note:path})});
    return routeLink(a,()=>{if(folder)openFolder(path,true,collection);else openNote(path);});
  }
  return {
    render(data, nextProject, userID) {
      snapshot=data;project=nextProject;user=userID;
      let view='list';try {const saved=localStorage.getItem(preferenceKey());if(['list','cards'].includes(saved))view=saved;}catch{}
      setView(view);
      const collection = ({notes:'Notes',drafts:'Drafts',archives:'Archives'})[snapshot.collection] || project.name;
      $('folder-title').textContent=snapshot.path.split('/').pop() || (snapshot.collection==='notes' ? project.name : collection);
      const notebooks=document.createElement('span');notebooks.textContent='Notebooks';const divider=document.createElement('span');divider.textContent='/';divider.setAttribute('aria-hidden','true');
      $('folder-crumbs').replaceChildren(notebooks,divider,link(project.name,'',true,'notes'));
      if(snapshot.collection && snapshot.collection!=='notes'){const sep=document.createElement('span');sep.textContent='/';sep.setAttribute('aria-hidden','true');$('folder-crumbs').append(sep,link(collection,'',true));}
      const parts=snapshot.path ? snapshot.path.split('/') : [];
      parts.forEach((name,index)=>{const sep=document.createElement('span');sep.textContent='/';sep.setAttribute('aria-hidden','true');$('folder-crumbs').append(sep,link(name,parts.slice(0,index+1).join('/'),true));});
      const overview=snapshot.items.find(item=>item.kind==='note' && item.path===(snapshot.path?snapshot.path+'/':'')+'index.md');
      $('folder-overview').replaceChildren();
      if(overview)$('folder-overview').append(link('Overview',overview.path,false));
      const items=$('folder-items'),folders=$('folder-groups');items.replaceChildren();folders.replaceChildren();
      for(const item of snapshot.items) {
        const row=document.createElement('li');row.className='browse-item';row.dataset.kind=item.kind;
        const a=link('',item.path,item.kind==='folder');a.className='browse-item-link';a.title=item.path;if(item.kind==='note')a.dataset.note=item.path;
        const imageURL=coverURL(project.id,item.metadata?.cover,snapshot.revision);
        if(imageURL && item.kind==='note') {const image=document.createElement('img');image.src=imageURL;image.alt='';image.loading='lazy';image.className='card-cover';image.onerror=()=>image.remove();a.append(image);}
        const symbol=icon(item.kind==='folder'?'folder':'document');symbol.classList.add('row-icon');a.append(symbol);
        const text=document.createElement('div');text.className='card-text';
        const title=document.createElement('span');title.className='card-title';title.textContent=item.title;
        text.append(title);
        const preview=document.createElement('p');preview.className='card-excerpt';preview.textContent=item.kind==='folder' ? item.count+' '+(item.count===1?'note':'notes') : item.excerpt || '';text.append(preview);
        a.append(text);
        if(item.metadata?.status || item.metadata?.tags?.length){const tags=document.createElement('div');tags.className='card-tags';for(const value of [item.metadata.status,...(item.metadata.tags||[]).map(tag=>'#'+tag)].filter(Boolean).slice(0,4)){const tag=document.createElement('span');tag.textContent=value;if(value===item.metadata.status)tag.className='card-status';tags.append(tag);}a.append(tags);}
        if(item.kind==='folder'){const arrow=icon('chevron');arrow.classList.add('folder-chevron');a.append(arrow);}
        row.append(a);setupActions(row,{kind:item.kind,path:item.path,name:item.title});(item.kind==='folder'?folders:items).append(row);
      }
      $('folder-group').hidden=!folders.children.length;
      // Subfolders alone are not an empty folder; a bare Notes heading would contradict them.
      const onlyFolders=!items.children.length && !!folders.children.length;
      $('library-notes-label').hidden=onlyFolders;
      $('folder-empty').hidden=!!items.children.length || onlyFolders;
      // New notes start in Notes or Drafts; Archives only receives notes moved there.
      document.querySelector('#folder-browser .creation-row').hidden=snapshot.collection==='archives';
      $('folder-count').textContent=items.children.length+' '+(items.children.length===1?'note':'notes')+' · '+folders.children.length+' '+(folders.children.length===1?'folder':'folders');
    }
  };
}
