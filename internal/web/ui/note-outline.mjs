// The headings of a note's Markdown, for Contents while editing, when the
// hidden reading view is not rendered (plan risk 18). One pass over the
// lines finds ATX and setext headings outside front matter (which the server
// leaves out of the reading view), fenced and indented code and HTML blocks,
// as the reader shows them; headings nested in a block quote or a list are
// not listed. Each heading is {level, text, from}: `from` is the offset of
// its first line in the Markdown and `text` its plain text. A level 1 heading
// that is the body's first element is the `title`, which the reader shows
// above the sections; `headings` are the others.
export function noteOutline(markdown) {
  const headings=[];
  let title=null,first=true,paragraph=null,fence=null,html=null,container=null;
  const add=(heading,isTitle)=>{if(isTitle && heading.level===1 && !title)title=heading;else headings.push(heading)};
  for(let at=bodyStart(markdown);at<=markdown.length;){
    const from=at,newline=markdown.indexOf('\n',at),end=newline<0?markdown.length:newline;
    let line=markdown.slice(from,end);
    if(line.endsWith('\r'))line=line.slice(0,-1);
    at=end+1;
    if(fence){
      if(closesFence(line,fence))fence=null;
      continue;
    }
    const blank=/^[ \t]*$/.test(line);
    if(html){
      if(html==='blank' ? blank : html.test(line))html=null;
      continue;
    }
    if(blank){
      paragraph=null;
      // A list item starts with one blank line at most.
      if(container?.empty)container=null;
      else if(container)follow(container,0,'');
      continue;
    }
    const [indent,rest]=leading(line,0),isFirst=first;
    if(container){
      // Quoted lines, and lines indented to the item's content, are in it.
      if(container.quote ? indent<4 && rest[0]==='>' : indent>=container.column){
        if(container.quote)follow(container,...quoteContent(rest,indent));
        else {container.empty=false;follow(container,indent-container.column,rest)}
        continue;
      }
      // Another line continues its last paragraph, if it has one, unless it
      // starts a block that interrupts a paragraph; it ends the container
      // otherwise, as in the server's Markdown (goldmark).
      if(container.lazy && (indent>=4 || !interrupts(rest)))continue;
      container=null;
    }
    if(indent>=4){
      if(paragraph)paragraph.lines.push(rest);
      else first=false;
      continue;
    }
    let match;
    if((match=fenceOpening(rest))){fence=match;paragraph=null;first=false;continue}
    if((match=/^(#{1,6})(?:[ \t](.*))?$/.exec(rest))){
      add({level:match[1].length,text:plain(atxText(match[2] || '')),from},isFirst);
      paragraph=null;first=false;continue;
    }
    if(paragraph && /^(?:=+|-+)[ \t]*$/.test(rest)){
      // A table in the paragraph ends its text; when the table is all of it,
      // the line starts a block of its own.
      const lines=paragraph.lines.slice(0,tableStart(paragraph.lines));
      if(lines.length){
        add({level:rest[0]==='='?1:2,text:plain(lines.join(' ')),from:paragraph.from},paragraph.first);
        paragraph=null;continue;
      }
      paragraph=null;
    }
    if(thematicBreak.test(rest)){paragraph=null;first=false;continue}
    if(rest[0]==='>'){
      container={quote:true};follow(container,...quoteContent(rest,indent));
      paragraph=null;first=false;continue;
    }
    // A list interrupts a paragraph only when it is a bullet or starts at 1
    // and has text; otherwise its line continues the paragraph.
    if((paragraph ? interruptingItem : listMarker).test(rest)){
      const item=listItem(rest,indent);
      container={column:item.column,empty:!item.text};follow(container,item.indent,item.text);
      paragraph=null;first=false;continue;
    }
    if(rest[0]==='<'){
      const ending=htmlEnd(rest,!!paragraph);
      if(ending){
        if(ending==='blank' || !ending.test(rest))html=ending;
        // The reader leaves HTML out, so the block after it can be the first.
        paragraph=null;continue;
      }
    }
    if(paragraph)paragraph.lines.push(rest);
    else {paragraph={from,lines:[rest],first:isFirst};first=false}
  }
  return {title,headings};
}

const listMarker=/^(?:[-+*]|\d{1,9}[.)])(?:[ \t]|$)/,interruptingItem=/^(?:[-+*]|1[.)])[ \t]+\S/;
const thematicBreak=/^([-*_])(?:[ \t]*\1){2,}[ \t]*$/,atxHeading=/^#{1,6}(?:[ \t]|$)/;

// A line's indent in columns, from the column it starts at, and its text.
function leading(line,column) {
  let indent=0,i=0;
  for(;i<line.length && (line[i]===' ' || line[i]==='\t');i++)indent+=line[i]==='\t'?4-(column+indent)%4:1;
  return [indent,line.slice(i)];
}

// The content of a quoted line: its indent after the > and the space that
// may follow it, and its text.
function quoteContent(rest,indent) {
  const [space,text]=leading(rest.slice(1),indent+1);
  return [Math.max(0,space-1),text];
}

// A list item's line: the column its content starts at (after the marker
// and the spaces that follow it, or one space after the marker when the item
// starts blank or with indented code), and its content's indent and text.
function listItem(rest,indent) {
  const marker=/^(?:[-+*]|\d{1,9}[.)])/.exec(rest)[0].length,[space,text]=leading(rest.slice(marker),indent+marker);
  const padding=!text || space>4 ? 1 : space;
  return {column:indent+marker+padding,indent:Math.max(0,space-padding),text};
}

// Follows a line's content in a block quote ({quote:true}) or list item
// ({column, empty}: where its content starts, and whether it has none yet).
// The container's `lazy` is set while its last block is a paragraph, which a
// line outside it can continue, and its `fence` and `html` while a fence or
// HTML block in it is open. Quotes and items nested in it are followed to
// their content.
function follow(container,indent,text) {
  for(;;){
    if(container.fence){
      if(closesFence(' '.repeat(Math.min(indent,4))+text,container.fence))container.fence=null;
      return;
    }
    if(container.html){
      if(container.html==='blank' ? !text : container.html.test(text))container.html=null;
      return;
    }
    if(!text){container.lazy=false;return}
    // Indented code, or a paragraph's next line.
    if(indent>=4)return;
    let match;
    if(atxHeading.test(text) || thematicBreak.test(text) || (container.lazy && /^(?:=+|-+)[ \t]*$/.test(text))){container.lazy=false;return}
    if((match=fenceOpening(text))){container.fence=match;container.lazy=false;return}
    if(text[0]==='>'){container.lazy=false;[indent,text]=quoteContent(text,indent);continue}
    if((container.lazy ? interruptingItem : listMarker).test(text)){
      container.lazy=false;({indent,text}=listItem(text,indent));continue;
    }
    if(text[0]==='<'){
      const ending=htmlEnd(text,!!container.lazy);
      if(ending){
        if(ending==='blank' || !ending.test(text))container.html=ending;
        container.lazy=false;return;
      }
    }
    container.lazy=true;return;
  }
}

// Whether a line outside a paragraph's container starts a block of its own:
// as goldmark opens them there, any list item does, and HTML that may
// interrupt a paragraph.
function interrupts(rest) {
  return atxHeading.test(rest) || !!fenceOpening(rest) || thematicBreak.test(rest) || rest[0]==='>'
    || listMarker.test(rest) || (rest[0]==='<' && !!htmlEnd(rest,true));
}

// The run of backticks or tildes that opens a fence on this line, or null.
function fenceOpening(rest) {
  const match=/^(`{3,}|~{3,})(.*)$/.exec(rest);
  return match && !(match[1][0]==='`' && match[2].includes('`')) ? match[1] : null;
}

// Whether a line closes the fence opened by this run.
function closesFence(line,run) {
  const close=/^ {0,3}(`{3,}|~{3,})[ \t]*$/.exec(line);
  return !!close && close[1][0]===run[0] && close[1].length>=run.length;
}

// Where a table starts in a paragraph's lines (its header row), or their
// length when there is none, as goldmark's GFM tables find one: the first
// delimiter row follows a row with no more cells than it has columns.
function tableStart(lines) {
  for(let i=1;i<lines.length;i++){
    const columns=delimiterColumns(lines[i]);
    if(columns)return rowCells(lines[i-1])<=columns ? i-1 : lines.length;
  }
  return lines.length;
}
function delimiterColumns(line) {
  if(!/^[ \t|:-]*$/.test(line) || /^-*$/.test(line))return 0;
  let columns=line.split('|');
  if(!columns[0].trim())columns=columns.slice(1);
  if(columns.length && !columns.at(-1).trim())columns=columns.slice(0,-1);
  return columns.every(column=>/^\s*:?-+:?\s*$/.test(column)) ? columns.length : 0;
}
function rowCells(line) {
  let row=line.trim();
  if(row.startsWith('|'))row=row.slice(1);
  if(row.endsWith('|'))row=row.slice(0,-1);
  return row ? row.split(/(?<!\\)\|/).length : 0;
}

// Where the body starts: after front matter, as the server finds it (a
// first line of exactly ---, closed by a --- line within 64 KiB).
function bodyStart(markdown) {
  const opening=markdown.startsWith('---\n')?4:markdown.startsWith('---\r\n')?5:0;
  for(let at=opening;opening && at<markdown.length && at<=65536;){
    const newline=markdown.indexOf('\n',at),end=newline<0?markdown.length:newline+1;
    if(markdown.slice(at,end).replace(/[\r\n]+$/,'')==='---')return end;
    at=end;
  }
  return 0;
}

// An ATX heading's content without its closing sequence of #.
function atxText(content) {
  content=content.trim();
  return /^#+$/.test(content)?'':content.replace(/[ \t]+#+$/,'');
}

// The end of an HTML block starting on this line, as goldmark finds them: a
// pattern for the line that closes it, 'blank' for a blank line, or null
// when it is no block. A lone tag other than a block tag (type 7) does not
// interrupt a paragraph.
function htmlEnd(line,inParagraph) {
  if(/^<(?:script|pre|style|textarea)(?:\s|>|\/>|$)/i.test(line))return /<\/(?:script|pre|style|textarea)>/i;
  if(line.startsWith('<!--'))return /-->/;
  if(line.startsWith('<?'))return /\?>/;
  if(/^<![A-Z]/.test(line))return />/;
  if(line.startsWith('<![CDATA['))return /\]\]>/;
  const lone=loneTag.exec(line),tag=lone || /^<(?:\/ *)?()([a-zA-Z][a-zA-Z0-9-]*)(?: .*|>.*|\/>.*|)$/.exec(line);
  if(tag && blockTags.has(tag[2].toLowerCase()))return 'blank';
  if(lone && !inParagraph && !/^(?:script|style|pre)$/i.test(lone[2]) && !(lone[1] && lone[3]))return 'blank';
  return null;
}
const blockTags=new Set(('address article aside base basefont blockquote body caption center col colgroup dd details dialog dir div dl dt '
  +'fieldset figcaption figure footer form frame frameset h1 h2 h3 h4 h5 h6 head header hr html iframe legend li link main menu menuitem '
  +'meta nav noframes ol optgroup option p param search section summary table tbody td tfoot th thead title tr track ul').split(' '));
// A line of one opening or closing tag, with its attributes.
const loneTag=new RegExp('^<(/ *)?([a-zA-Z][a-zA-Z0-9-]*)((?:[ \\t]+[a-zA-Z_:][a-zA-Z0-9:._-]*(?:[ \\t]*=[ \\t]*(?:[^"\'=<>`\\x00-\\x20]+|\'[^\']*\'|"[^"]*"))?)*) *(?:>|/>) *$');

// A heading's inline Markdown as plain text, close to what the reader shows:
// code spans keep their text, links and images their label, and emphasis,
// tags and escapes their marks dropped.
function plain(text) {
  // Code spans and escaped characters are kept aside from the other rules.
  const kept=[],keep=value=>{kept.push(value);return '\u0000'+(kept.length-1)+'\u0000'};
  text=text
    .replace(/(?<!`)(`+)(?!`)(.*?[^`])\1(?!`)/g,(_,ticks,inner)=>keep(inner.trim()))
    .replace(/\\([!-/:-@[-`{-~])/g,(_,character)=>keep(character))
    .replace(/\[\[([^\]|]*)\|([^\]]*)\]\]/g,'$2')
    .replace(/\[\[([^\]]*)\]\]/g,'$1')
    .replace(/!?\[([^\]]*)\](?:\((?:[^()]|\([^()]*\))*\)|\[[^\]]*\])/g,'$1')
    .replace(/<\/?[A-Za-z][^>]*>/g,'');
  for(let pass=0;pass<2;pass++)
    text=text
      .replace(/(\*{1,3}|~~)(?=\S)(.*?\S)\1/g,'$2')
      .replace(/(?<![\p{L}\p{N}_])(_{1,3})(?=\S)(.*?\S)\1(?![\p{L}\p{N}_])/gu,'$2');
  return text
    .replace(/\u0000(\d+)\u0000/g,(_,index)=>kept[index])
    .replace(/\s+/g,' ')
    .trim();
}
