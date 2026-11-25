const copy=value=>structuredClone(value);
export function createTurnSubmission(uuid = () => crypto.randomUUID()) {
  let pending = null;
  return {
    prepare(text,options,attachments) {
      const payload = {text,options,attachments};
      if (pending && JSON.stringify(payload) !== JSON.stringify({text:pending.text,options:pending.options,attachments:pending.attachments})) throw Error('Retry the original send before changing its message or options.');
      pending ||= {submissionId:uuid(),...copy(payload)};
      return copy(pending);
    },
    current: () => pending && copy(pending),
    confirmed() { pending = null; },
  };
}
export function attachmentSnapshot(kind, note, getDraft) {
  if (!note?.path) throw Error('Open a note to attach it.');
  if (kind === 'private') {
    const draft = getDraft();
    if (!draft || typeof draft.content !== 'string' || draft.path !== note.path) throw Error('The note changed. Choose the attachment again.');
    return {kind,path:draft.path,revision:draft.revision || '',content:draft.content};
  }
  if (!['committed','live'].includes(kind) || !note.revision) throw Error('Save this note before attaching a committed or shared version.');
  const result = {kind,path:note.path,revision:note.revision};
  if (kind === 'live' && note.liveRevision) result.liveRevision = note.liveRevision;
  return result;
}

// Drafts live only in the current account/notebook session. Retiring a server
// conversation preserves its unsent text or attachments under an orphan key.
export function createComposerDrafts() {
  const drafts=new Map();
  const fresh=()=>({text:'',attachments:[],submission:createTurnSubmission()});
  return {
    select:key=>drafts.get(key) || fresh(),
    remember:(key,draft)=>drafts.set(key,draft),
    discard:key=>drafts.delete(key),
    entries:()=>drafts.entries(),
    clear:()=>drafts.clear(),
    retire(id) {
      const draft=drafts.get(id);drafts.delete(id);
      if(!draft)return '';
      draft.submission.confirmed();
      if(!draft.text&&!draft.attachments.length)return '';
      const key='draft:'+id;drafts.set(key,draft);return key;
    },
  };
}
