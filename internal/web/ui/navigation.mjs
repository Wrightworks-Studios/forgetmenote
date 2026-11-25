export function navigationURL({project, collection, layout, note, noteId, folder, revision, hash, origin, source, path, startLine, endLine, cursor}) {
  const query = new URLSearchParams({project, collection});
  if (layout) query.set('layout', layout);
  if (note !== undefined) query.set('note', note);
  if (note !== undefined && noteId) query.set('noteId', noteId);
  if (folder !== undefined) query.set('folder', folder);
  if (revision) query.set('revision', revision);
  for (const [key,value] of Object.entries({origin,source,path,startLine,endLine,cursor})) if(value)query.set(key,value);
  return '/?' + query + (hash ? '#' + hash : '');
}
// The one rule for every route the app publishes (notes and folders, search,
// Inbox, settings pages): an address already shown is left alone; a new
// history entry is added only when asked for, once the initial route is
// installed and while no Back/Forward restoration is running; otherwise the
// current entry is replaced.
export function routeMethod(from, to, {push = false, ready = true, restoring = false} = {}) {
  if (from === to) return null;
  return push && ready && !restoring ? 'pushState' : 'replaceState';
}
