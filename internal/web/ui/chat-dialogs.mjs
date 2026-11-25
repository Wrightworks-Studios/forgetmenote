import {element,button,field} from './dom.mjs';
import {openDialog,dialogShell} from './overlay.mjs';
import {attachmentSnapshot} from './chat-composer.mjs';

const chatDialog = (panel,label) => dialogShell(label,{className:'chat-dialog',heading:'chat-heading',host:panel,transient:true});
export function openAttachmentPicker({panel,note,getNote,getDraftAttachment,scope,onAttach,showError}) {
    if (!note?.path) {showError(Error('Open a note, then choose the version to attach.'));return;}
    const path=note.path,dialog=chatDialog(panel,'Attach a note version');
    dialog.dataset.attachmentPicker = '';
    dialog.append(element('p',path),element('p','Shared with your model when you send the message.'));
    for (const [kind,label,description] of [['committed','Committed version','The saved Git version.'],['live','Shared live version','The latest shared edits.'],['private','Private draft snapshot','Includes unsaved edits from this device.']]) {
      const choice = button(label,() => {
        try {
          if (!scope.active() || getNote()?.path !== path) throw Error('The note changed. Choose the attachment again.');
          const snapshot = attachmentSnapshot(kind,note,getDraftAttachment);
          onAttach(snapshot);dialog.close();
        } catch (problem) {dialog.append(element('p',problem.message,{role:'alert'}));}
      },{'data-attachment-kind':kind});
      choice.disabled = kind !== 'private' && !note.revision;
      dialog.append(choice,element('p',description,{class:'small'}));
    }
    openDialog(dialog);
  }
export function openTranscriptSave({panel,chat,endpoint,scope,post,onSaved,showError,turnID,returnFocus}) {
    if(!chat)return;
    const dialog=chatDialog(panel,'Save chat to a note'),selected=new Set();
    const path = element('input',undefined,{id:'chat-save-path',value:'Chats/' + new Date().toISOString().slice(0,10) + '-' + chat.id.slice(0,8) + '.md'});
    dialog.append(element('p','Select up to 100 messages to save in a note shared with notebook members.'),field('New note path',path));
    const rows = element('div',undefined,{class:'chat-save-turns'}), preview = element('pre','',{id:'chat-save-preview'});
    const lastAnswer = [...chat.turns].reverse().find(turn => turn.role === 'assistant' && !turn.intermediate);
    const update = () => {preview.textContent = chat.turns.filter(turn => selected.has(turn.id)).map(turn => '## ' + (turn.role === 'user' ? 'You' : 'Assistant') + '\n\n' + turn.content).join('\n\n');};
    for (const turn of chat.turns) {
      const check = element('input',undefined,{type:'checkbox',value:turn.id});
      if (turnID ? turn.id === turnID : turn === lastAnswer) {check.checked = true;selected.add(turn.id);}
      check.onchange = () => {check.checked ? selected.add(turn.id) : selected.delete(turn.id);update();};
      rows.append(field((turn.role === 'user' ? 'You: ' : 'Assistant: ') + turn.content.slice(0,120),check));
    }
    dialog.append(rows,element('h3','Selected transcript preview'),preview);update();
    const failure = element('p','',{role:'alert'});let pending = null;
    const submitSave = button('Save selected messages',async () => {
      if(!scope.active()){dialog.close();return;}
      submitSave.disabled = true;failure.textContent = '';
      try {
        if (!selected.size || selected.size > 100) throw Error('Choose between 1 and 100 messages.');
        if (!path.value.trim()) throw Error('Enter a new note path.');
        pending ||= {submissionId:crypto.randomUUID(),path:path.value.trim(),revision:'',turnIds:chat.turns.filter(turn => selected.has(turn.id)).map(turn => turn.id)};
        path.disabled = true;rows.querySelectorAll('input').forEach(check => check.disabled = true);
        const result = await post(endpoint,pending,{signal:scope.signal});
        if (!scope.active()) return;
        onSaved(result);dialog.close();
      } catch (problem) {
        if (!scope.active()) return;
        if ([401,403].includes(problem.status)) {showError(problem);return;}
        if (problem.status && problem.status < 500) {pending = null;path.disabled = false;rows.querySelectorAll('input').forEach(check => check.disabled = false);}
        failure.textContent = problem.message;
      } finally {submitSave.disabled = false;submitSave.textContent = pending ? 'Retry save' : 'Save selected messages';}
    });dialog.append(failure,submitSave);openDialog(dialog,{opener:returnFocus});
  }
