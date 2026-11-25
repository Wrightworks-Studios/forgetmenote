import {element} from './dom.mjs';
import {notebookPath} from './request.mjs';

// A conversation's rendered rows and bounded preview cache share its access
// lifetime. Server HTML retains the application's sanitizer and CSP contract.
export function createTranscriptView({viewport,context,post,messageActions,sourceDetails,installAnswer,showError}) {
  let rowsByID=new Map();const signatures=new WeakMap(),renderQueue=new Map();
  function render(chat) {
    const scope=context();
    const existing=rowsByID;const nextRows=new Map();
    const rows=[];
    for (const turn of chat?.turns || []) {
      // Tool-bearing rounds are progress, not additional answers. Their durable
      // text remains available in the run's Activity disclosure.
      if (turn.role === 'assistant' && turn.intermediate) continue;
      let row = existing.get(turn.id);
      const signature=JSON.stringify([turn.role,turn.content,!!turn.streaming,turn.citations||[],turn.attachments||[],turn.runId,!!chat?.runs?.some(run=>run.id===turn.runId)]);
      if (row && signatures.get(row)===signature) {rows.push(row);nextRows.set(turn.id,row);continue;}
      if (!row) row = element('article',undefined,{'data-turn':turn.id});
      row.className = 'chat-turn chat-turn-' + (turn.role === 'user' ? 'user' : 'assistant');row.dataset.runId = turn.runId || '';
      row.replaceChildren();signatures.set(row,signature);
      const heading = element('div',undefined,{class:'chat-turn-heading'});heading.append(element('h3',turn.role === 'user' ? 'You' : 'Assistant',{class:'chat-role'}));
      if (!turn.streaming && turn.content) heading.append(messageActions(turn));
      row.append(heading);
      const content = element('div',turn.content,{class:'chat-content'});row.append(content);
      for (const item of turn.attachments || []) row.append(element('p',`${item.kind === 'private' ? 'Private draft' : item.kind === 'live' ? 'Shared live' : 'Committed'} attachment · ${item.path}`,{class:'small'}));
      // Legacy/unassociated turns still retain their sources alongside the text.
      if (!chat?.runs?.some(run => run.id === turn.runId)) {
        const sources = sourceDetails(turn.citations || []);if (sources) row.append(sources);
      }
      rows.push(row);nextRows.set(turn.id,row);
      if (turn.role === 'assistant' && !turn.streaming && turn.content) {
        const key=turn.content, prefix=notebookPath(scope.project);
        let task = renderQueue.get(key);
        if (!task) {task = post(prefix + '/api/preview',{content:turn.content},{signal:scope.signal});renderQueue.set(key,task);if(renderQueue.size>64)renderQueue.delete(renderQueue.keys().next().value);}
        task.then(result => {
          if (!scope.active() || !row.isConnected || signatures.get(row)!==signature) return;
          const nearBottom = viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight < 90;
          installAnswer(content,result.html,turn);content.classList.add('chat-markdown');
          if (nearBottom) viewport.scrollTop = viewport.scrollHeight;
        }).catch(problem => {if (scope.active() && [401,403].includes(problem.status)) showError(problem);});
      }
    }
    rowsByID=nextRows;return rows;
  }
  return {render,reset(){rowsByID.clear();renderQueue.clear();}};
}
