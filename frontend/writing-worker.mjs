import {analyzeWriting,projectWriting} from './writing-model.mjs';
import {anchorOf,placeAnchors,renderedText} from './rendered-text.mjs';
// Only the active document is retained. Switching documents terminates this
// worker; obsolete results are also fenced by the UI's generation. The note
// editor runs its own copy to place comment passages ({kind: 'comments'})
// and to make the anchor of a selection ({kind: 'anchor'}) in the note's
// rendered text, which it keeps for the text it last read.
let cachedSource,projection,renderedSource,rendered;
const read=source=>{if(source!==renderedSource){rendered=renderedText(source);renderedSource=source;}return rendered;};
self.onmessage=({data})=>{
 const {id,kind,source}=data;
 try{
  if(kind==='comments')self.postMessage({id,ranges:[...placeAnchors(read(source),data.comments)].map(([comment,range])=>({id:comment,...range}))});
  else if(kind==='anchor')self.postMessage({id,anchor:anchorOf(read(source),data.from,data.to)});
  else {
   if(source!==cachedSource){projection=projectWriting(source);cachedSource=source;}
   self.postMessage({id,result:analyzeWriting(source,data.options,projection)});
  }
 }catch(error){self.postMessage({id,error:error.message||'Writing analysis failed.'});}
};
