import {element as node} from './dom.mjs';

// Choices for settings that name notebooks, accounts or connections by ID. The
// server checks every saved value; these controls only offer the current ones.
// A saved value that is no longer offered stays chosen, marked, until someone
// changes it, so opening and saving a form never drops what was there.

// The order a saved list keeps when chosen again: values still chosen stay where
// they were, and new ones follow in the order offered.
export function keepOrder(saved=[],chosen=[]){
  return [...saved.filter(value=>chosen.includes(value)),...chosen.filter(value=>!saved.includes(value))];
}

// Sets a select to `value`, adding a marked option when it is not offered.
export function chooseOption(select,value,missing=value=>`${value} (not found)`){
  value=value??'';
  if(![...select.options].some(option=>option.value===value))select.append(new Option(missing(value),value));
  select.value=value;
}

// A select of `options` ([value, text] pairs) after a placeholder.
export function choiceSelect(label,options,{placeholder}={}){
  const select=node('select');select.setAttribute('aria-label',label);
  if(placeholder!==undefined)select.append(new Option(placeholder,''));
  for(const [value,text] of options)select.append(new Option(text,value));
  return select;
}

// Checkboxes for choosing several values, in a fieldset whose `disabled`
// switches them all. set(values) shows a saved list; read() returns the chosen
// values in the saved order.
export function checkList(legend,options,{missing=value=>`${value} (not found)`,empty='None available.'}={}){
  const box=node('fieldset',undefined,'check-list'),list=node('div',undefined,'check-list-options');
  box.append(node('legend',legend),list);
  let saved=[];
  function set(values=[]){
    saved=[...values];
    const offered=[...options,...values.filter(value=>!options.some(([option])=>option===value)).map(value=>[value,missing(value)])];
    list.replaceChildren(...offered.map(([value,text])=>{
      const label=node('label',undefined,'check-row'),input=node('input');
      input.type='checkbox';input.value=value;input.checked=values.includes(value);
      label.append(input,node('span',text));return label;
    }));
    if(!offered.length)list.append(node('p',empty,'small'));
  }
  const read=()=>keepOrder(saved,[...list.querySelectorAll('input:checked')].map(input=>input.value));
  set();
  return {element:box,set,read};
}

// Notebook choices from the administration directory. Private inboxes and
// assistant notebooks belong to one person and are not offered.
export const notebookChoices=projects=>(projects||[]).filter(p=>!p.protectedInbox&&!p.assistantOwner).map(p=>[p.id,p.name&&p.name!==p.id?`${p.name} (${p.id})`:p.id]);
export const accountName=user=>user.displayName||user.id;
