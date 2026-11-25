import {openDialog} from './overlay.mjs';
import {attachmentMarkdown,videoMarkdown} from './media.mjs';
import {notebookPath} from './request.mjs';
import {$} from './dom.mjs';

// Inserting attachments, videos and notebook links into the open note. The
// caller owns the editor, drafts (`remember`) and saves (`beginWrite`/`endWrite`).
// The toolbar's Insert menu starts an insertion through insert(kind).
export function initMediaInsert({api,notice,state,getContext,noteEditor,remember,countWords,preview,setMode,beginWrite,endWrite,openNotebookLink}) {
  // A block of its own after the caret's line, with the note's line ends, as
  // one undo step. The commands load with the editor, so this import is
  // already there.
  async function insertBlock(markdown) {
    const {blockInsertion,stringDoc}=await import('../../../frontend/markdown-commands.mjs');
    const {from,insert,caret}=blockInsertion(stringDoc(noteEditor.value),noteEditor.selectionEnd,[markdown]);
    noteEditor.undoControls?.boundary?.();
    noteEditor.setRangeText(insert,from,from,'end');
    noteEditor.undoControls?.boundary?.();
    noteEditor.selectRange(noteEditor.value,caret,caret);
  }
  function insertMedia(kind) {
    if(!getContext().current?.editable || getContext().saving)return;
    if(kind==='attachment') {$("upload").click();return;}
    $("video-url").value='';
    $("video-error").textContent='';
    openDialog($("video-dialog"));
    $("video-url").focus();
  }
  $("notebook-link-insert").onclick=()=>openNotebookLink();
  $("video-form").onsubmit=async event=>{
    event.preventDefault();
    if(!getContext().current?.editable || getContext().saving)return;
    let markdown;
    try {markdown=videoMarkdown($("video-url").value);}
    catch(error) {$("video-error").textContent=error.message;return;}
    $("video-dialog").close();
    await insertBlock(markdown);
    remember();countWords();preview();noteEditor.focus();
  };
  $("upload").onchange = async () => {
    const file = $("upload").files[0];
    if (!file || getContext().saving || !getContext().current?.editable) return;
    beginWrite();
    noteEditor.readOnly = true;
    const body = new FormData();
    body.append("file", file);
    if (getContext().current?.revision) body.append("revision", getContext().current.revision);
    state("Uploading attachment…");
    try {
      const result = await api(notebookPath(getContext().project.id)+"/api/upload", { method: "POST", body });
      if (getContext().current) {
        const insert = attachmentMarkdown(result.path,file.name,file.type);
        if (getContext().current.editable) {
          await insertBlock(insert);
          remember();
          setMode(true);
          countWords();
          await preview();
        }
      }
      state("Attachment committed · save the note to keep its link");
    } catch (e) {
      notice(e.message);
      state("Upload failed");
    } finally {
      endWrite();
      noteEditor.readOnly = !getContext().current?.editable;
      $("upload").value = "";
      countWords();
    }
  };
  return {insert:insertMedia};
}
