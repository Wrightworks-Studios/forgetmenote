import {Compartment,EditorState} from '@codemirror/state';
import {EditorView,lineNumbers,highlightActiveLineGutter} from '@codemirror/view';
import {syntaxHighlighting,HighlightStyle} from '@codemirror/language';
import {tags} from '@lezer/highlight';
import {SpaceRun} from './markdown-syntax.mjs';

// Reuse the application's contrast-tested colors across all display themes.
const sourceColors=HighlightStyle.define([
 {tag:[tags.keyword,tags.typeName,tags.function(tags.variableName)],color:'var(--accent)'},
 {tag:[tags.string,tags.number,tags.bool,tags.regexp],color:'var(--success)'},
 {tag:tags.comment,color:'var(--muted)',fontStyle:'italic'},
]);
// Each language pack is its own chunk, loaded only for a file that needs it.
const javascript=options=>()=>import('@codemirror/lang-javascript').then(pack=>pack.javascript(options));
const languages={
 py:()=>import('@codemirror/lang-python').then(pack=>pack.python()),
 js:javascript(),mjs:javascript(),jsx:javascript({jsx:true}),ts:javascript({typescript:true}),tsx:javascript({typescript:true,jsx:true}),
 go:()=>import('@codemirror/lang-go').then(pack=>pack.go()),
 json:()=>import('@codemirror/lang-json').then(pack=>pack.json()),
 yml:()=>import('@codemirror/lang-yaml').then(pack=>pack.yaml()),yaml:()=>import('@codemirror/lang-yaml').then(pack=>pack.yaml()),
 md:()=>import('@codemirror/lang-markdown').then(pack=>pack.markdown({extensions:[SpaceRun]})),
};
export function languageFor(path){return languages[path.split('.').pop().toLowerCase()] || null}
// The code shows at once; its highlighting follows when the pack has loaded.
export function readCode(root,doc,selected) {
 const language=new Compartment(),load=languageFor(doc.path);
 const view=new EditorView({parent:root,state:EditorState.create({doc:doc.content,extensions:[
  EditorView.cspNonce.of(document.querySelector('meta[name="source-style-nonce"]')?.content||''),
  EditorState.readOnly.of(true),EditorView.editable.of(false),EditorView.contentAttributes.of({'aria-label':'Read-only source code',tabindex:'0'}),
  lineNumbers({formatNumber:n=>String(n+(doc.startLine||1)-1)}),highlightActiveLineGutter(),syntaxHighlighting(sourceColors),language.of([]),
  EditorView.updateListener.of(update=>{if(update.selectionSet){const range=update.state.selection.main;selected({startLine:update.state.doc.lineAt(range.from).number+(doc.startLine||1)-1,endLine:update.state.doc.lineAt(range.to).number+(doc.startLine||1)-1});}}),
  EditorView.theme({'&':{backgroundColor:'transparent',color:'inherit'},'.cm-scroller':{overflow:'auto',fontFamily:'ui-monospace, monospace',lineHeight:'1.7'},'.cm-gutters':{backgroundColor:'transparent',color:'var(--muted)',border:'0'},'.cm-activeLineGutter':{backgroundColor:'var(--selection)',color:'var(--text)'},'.cm-content':{padding:'16px 0'},'.cm-line':{padding:'0 16px'},'&.cm-focused':{outline:'2px solid var(--accent)',outlineOffset:'-2px'}}),
 ]})});
 let destroyed=false;
 load?.().then(extension=>{if(!destroyed)view.dispatch({effects:language.reconfigure(extension)})},()=>{});
 return ()=>{destroyed=true;view.destroy()};
}
