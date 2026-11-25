// A long text kept in pieces of a few thousand characters, so that a change
// copies only the pieces it touches. A JavaScript string never changes, and
// V8 copies a string built by concatenation whole the first time anything
// slices or searches it: editing a 1.2 MB note by slicing its text copied
// the whole note on every keystroke (0.6 ms and 2.4 MB of garbage each).
// `text` joins the pieces without copying them; only reading its characters
// copies it, once.
const size=8192;
export class TextPieces {
  constructor(text=''){this.reset(text)}
  reset(text){
    this.pieces=[];
    for(let at=0;at<text.length;at+=size)this.pieces.push(text.slice(at,at+size));
    this.length=text.length;this.joined=text;
  }
  get text(){
    if(this.joined===null){let text='';for(const piece of this.pieces)text+=piece;this.joined=text}
    return this.joined;
  }
  // Applies text changes {from, to, insert} in order, each to the text the
  // one before it left, and returns the changes that undo them. Changes that
  // do not fit throw a RangeError and change nothing.
  apply(changes){
    let length=this.length;
    for(const {from,to,insert} of changes){
      if(!Number.isInteger(from) || !Number.isInteger(to) || from<0 || to<from || to>length || typeof insert!=='string')
        throw new RangeError('Invalid text change '+JSON.stringify({from,to})+' for length '+length);
      length+=insert.length-(to-from);
    }
    const inverse=[];
    for(const {from,to,insert} of changes)inverse.push({from,to:from+insert.length,insert:this.replace(from,to,insert)});
    return inverse.reverse();
  }
  // Replaces from–to by insert and returns the text it replaced.
  replace(from,to,insert){
    const pieces=this.pieces;
    let first=0,start=0;
    while(first<pieces.length-1 && start+pieces[first].length<from)start+=pieces[first++].length;
    let last=first,end=start;
    while(last<pieces.length-1 && end+pieces[last].length<to)end+=pieces[last++].length;
    const head=pieces[first]??'',tail=pieces[last]??'';
    const removed=first===last?head.slice(from-start,to-start):head.slice(from-start)+pieces.slice(first+1,last).join('')+tail.slice(0,to-end);
    const middle=head.slice(0,from-start)+insert+tail.slice(to-end),replacement=[];
    if(middle.length>2*size)for(let at=0;at<middle.length;at+=size)replacement.push(middle.slice(at,at+size));
    else if(middle)replacement.push(middle);
    pieces.splice(first,last-first+1,...replacement);
    this.length+=insert.length-(to-from);this.joined=null;
    return removed;
  }
}
