export function noteTitle(note) {
  const filename=(note.path || '').split('/').pop().replace(/\.md$/i,'');
  return note.metadata?.title || note.title || (filename === 'index' ? 'Overview' : filename);
}

export function restoreDraft(saved, raw) {
  try {
    const draft = JSON.parse(raw);
    if (
      typeof draft?.content === "string" &&
      typeof draft?.revision === "string"
    )
      return { ...draft, conflict: typeof draft.version === "string" && typeof saved.version === "string" ? draft.version !== saved.version : draft.revision !== saved.revision };
  } catch {}
  return { ...saved, conflict: false };
}
export function resolveLink(source, href) {
  const notebook=notebookLink(href);
  if(notebook)return notebook;
  if (/^(https?:|mailto:)/i.test(href)) return { external: true, url: href };
  if (
    /^[a-z][a-z0-9+.-]*:/i.test(href) ||
    href.startsWith("//") ||
    href.includes("\\")
  )
    return null;
  const [raw, hash = ""] = href.split("#");
  let decoded;
  try {
    decoded = decodeURIComponent(raw.split("?")[0]);
  } catch {
    return null;
  }
  const parts = decoded.startsWith("/") ? [] : source.split("/").slice(0, -1);
  if (decoded === "") return { path: source, hash };
  for (const part of decoded.split("/")) {
    if (part === "..") {
      if (!parts.length) return null;
      parts.pop();
    } else if (part !== "" && part !== ".") {
      if (part.startsWith(".")) return null;
      parts.push(part);
    }
  }
  return { path: parts.join("/"), hash };
}

export function notebookLink(href, origin=globalThis.location?.origin) {
  try {
    const url=new URL(href,origin||'https://notebook.invalid');
    if(url.origin!==(origin||'https://notebook.invalid')||url.pathname!=='/'||url.username||url.password)return null;
    const project=url.searchParams.get('project'),path=url.searchParams.get('note')||'',noteId=url.searchParams.get('noteId')||'';
    if(!/^[a-z0-9][a-z0-9-]{0,63}$/.test(project||'')||(!path&&!noteId)||noteId&&!/^[a-f0-9]{32}$/.test(noteId))return null;
    return {project,path,noteId,revision:url.searchParams.get('revision')||'',hash:decodeURIComponent(url.hash.slice(1))};
  }catch{return null;}
}

export function movedPath(path, from, to) {
  return path === from ? to : path.startsWith(from + "/") ? to + path.slice(from.length) : path;
}

export function fileActionTarget(node, action, folder, name) {
  if (!["rename", "move", "duplicate"].includes(action)) return {error:"Choose an action."};
  const parts = node.path.split("/"), original = parts.pop();
  if (action === "rename") folder = parts.join("/");
  let filename = original;
  if (action !== "move") {
    name = name.trim();
    if (node.kind === "note") name = name.replace(/\.md$/i, "");
    if (!name || name.startsWith(".") || /[/\\\x00-\x1f]/.test(name)) return {error:"Enter a name without slashes or a leading dot."};
    filename = name + (node.kind === "note" ? ".md" : "");
  }
  if (folder && folder.split("/").some(part => !part || part.startsWith(".") || /[\\\x00-\x1f]/.test(part))) return {error:"Choose a valid folder."};
  const path = [folder, filename].filter(Boolean).join("/");
  if (path === node.path) return {error:action === "move" ? "Choose a different folder." : "Enter a different name."};
  if (node.kind === "folder" && path.startsWith(node.path + "/")) return {error:"Choose a destination outside this folder."};
  if (new TextEncoder().encode(path).length > 512) return {error:"This name and folder path are too long."};
  return {path};
}
export function destinationFolders(entries) {
  const folders = new Set([""]);
  for (const entry of entries) {
    if (!entry.path.endsWith(".md")) continue;
    const parts = entry.path.split("/");
    parts.pop();
    while (parts.length) {folders.add(parts.join("/")); parts.pop();}
  }
  return [...folders].sort((a,b) => a.localeCompare(b,undefined,{numeric:true}));
}
export function copyName(node, entries) {
  const parts = node.path.split("/"), filename = parts.pop();
  const stem = node.kind === "note" ? filename.slice(0,-3) : filename;
  const paths = [...entries.map(entry => entry.path), ...destinationFolders(entries)].map(path => path.toLowerCase());
  for (let number=1; ; number++) {
    const name = stem + " copy" + (number > 1 ? " " + number : "");
    const target = fileActionTarget(node,"duplicate",parts.join("/"),name).path;
    if (!target || !paths.includes(target.toLowerCase())) return name;
  }
}

export function blockingFileDrafts(action, from, to, drafts) {
  if (!action) return [];
  // Moving rewrites links throughout the project; copies only read the source.
  if (action !== "duplicate") return [...drafts];
  return drafts.filter(path => [from, to].some(root => path === root || path.startsWith(root + "/")));
}

export function contextMenuPosition(anchor, menu, viewport) {
  const left = Math.max(8, Math.min(anchor.right - menu.width, viewport.width - menu.width - 8));
  const below = anchor.bottom + 4;
  const top = below + menu.height <= viewport.height - 8 ? below : anchor.top - menu.height - 4;
  return {left, top: Math.max(8, Math.min(top, viewport.height - menu.height - 8))};
}
