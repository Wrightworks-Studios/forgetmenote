import {element,button} from './dom.mjs';
import {activeStates,canContinueRun} from './chat-model.mjs';

export function createRunView({runPanel,context,operationCard,sourceDetails,controlRun,mountCitationChecks}) {
  let rowsByID=new Map(),runSignature='';const signatures=new WeakMap();
  function render(chat) {
    const scope=context();
    const current=chat?.runs?.find(run=>activeStates.has(run.state)) || chat?.runs?.at(-1);
    const loaded = new Set((chat?.turns || []).map(turn => turn.runId));
    const existing=rowsByID;const nextRows=new Map();
    const rows=[];
    for (const run of chat?.runs || []) {
      const latest = run === chat.runs.at(-1);
      if (!latest && !loaded.has(run.id) && !canContinueRun(run) && run !== current) continue;
      const runTurns = (chat.turns || []).filter(turn => turn.runId === run.id);
      const updates = runTurns.filter(turn => turn.role === 'assistant' && turn.intermediate);
      const citations = [...run.citations || [],...runTurns.flatMap(turn => turn.citations || [])];
      const answered=runTurns.some(turn=>turn.role==='assistant'&&!turn.intermediate&&!turn.streaming);
      const signature = JSON.stringify([run,updates,citations,run === current,answered]);
      let row = existing.get(run.id);
      if (!row) row = element('section',undefined,{class:'chat-run','data-run':run.id});
      if (signatures.get(row)!==signature) {
        const activityOpen = !!row.querySelector('.chat-activity')?.open, sourcesOpen = !!row.querySelector('.chat-sources')?.open;
        const operationOpen = new Set([...row.querySelectorAll('[data-action] details[open]')].map(node => node.closest('[data-action]').dataset.action));
        signatures.set(row,signature);row.replaceChildren();
        const operations = (run.operations || []).filter(operation => operation.state !== 'awaiting_confirmation' || !operation.confirmation);
        const activity = element('details',undefined,{class:'chat-activity'});activity.open = activityOpen;
        activity.append(element('summary',`Activity${operations.length ? ' · ' + operations.length + ' actions' : ''} · ${(run.state || 'queued').replaceAll('_',' ')}`));
        if (run.error || run.message) activity.append(element('p',run.error || run.message,{class:'small'}));
        for (const turn of updates) activity.append(element('p',turn.content,{class:'chat-intermediate','data-update':turn.id}));
        for (const operation of operations) {
          const card = operationCard(operation,run);if (operationOpen.has(operation.id)) card.querySelector('details').open = true;activity.append(card);
        }
        if (run.review) {
          const notes = run.review.notes || [], examined = notes.filter(note => note.state === 'examined').length;
          activity.append(element('p',`${examined} of ${notes.length} notes examined · ${run.review.complete ? 'Review complete for eligible current notes at recorded revisions' : 'Partial review; coverage remains'}`,{class:'chat-coverage'}));
          const coverage = element('details');coverage.append(element('summary','Review coverage'));
          if (run.review.historyLimited) coverage.append(element('p','Historical comparisons are limited; completion covers eligible current notes only.',{class:'small'}));
          for (const note of notes) coverage.append(element('p',`${note.path} · ${note.state} · ${(note.revision || '').slice(0,7)}${note.total ? ' · ' + (note.cursor || 0) + '/' + note.total + ' passages examined' : ''}${note.summary ? ' — ' + note.summary : ''}`,{class:'small'}));
          activity.append(coverage);
        }
        if (run !== current && canContinueRun(run)) activity.append(button('Continue',() => controlRun(run,'resume'),{'data-run-control':'resume'}));
        row.append(activity);const sources = sourceDetails(citations,sourcesOpen);if (sources) row.append(sources);
        if(mountCitationChecks && run.state==='completed' && answered) {
          const host=element('div',undefined,{class:'chat-citation-checks'}),id=chat.id;row.append(host);
          queueMicrotask(()=>{if(scope.active() && host.isConnected)mountCitationChecks(host,{project:scope.project,chat:id,run:run.id,valid:()=>scope.active()});});
        }
      }
      rows.push(row);nextRows.set(run.id,row);
    }
    const decisions=(chat?.runs || []).flatMap(run=>(run.operations || []).filter(operation=>operation.confirmation&&operation.state==='awaiting_confirmation').map(operation=>({run,operation})));
    rowsByID=nextRows;const signature = JSON.stringify([current,decisions]);
    if (runSignature === signature) return rows;
    runSignature = signature;runPanel.replaceChildren();
    if (current) {
      const status = element('div',undefined,{class:'chat-run-status'});
      if (activeStates.has(current.state)) {
        status.append(element('span',current.state === 'awaiting_confirmation' ? 'Approval required' : 'Working…',{role:'status'}));
      }
      if (current.error || current.message && current.state !== 'completed') runPanel.append(element('p',current.error || current.message,{class:'chat-run-error',role:'status'}));
      if (canContinueRun(current)) {
        status.append(element('span',current.state === 'completed' ? 'Review has more to examine' : current.state === 'cancelled' ? 'Stopped' : 'Run ' + current.state));
        status.append(button('Continue',() => controlRun(current,'resume'),{id:'chat-continue','data-run-control':'resume'}));
      }
      if (current.state === 'uncertain') status.append(element('span','Outcome uncertain. Inspect the recorded actions and notebook before deciding how to proceed.'));
      if (status.childNodes.length) runPanel.append(status);
    }
    // Required decisions always sit outside collapsed Activity and Sources.
    for(const {run,operation} of decisions)runPanel.append(operationCard(operation,run));
    runPanel.hidden = !runPanel.childNodes.length;
    return rows;
  }
  return {render,invalidate(){runSignature='';},reset(){rowsByID.clear();runSignature='';}};
}
