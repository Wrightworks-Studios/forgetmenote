// Preview the same small reserved scalar syntax as workspace.ParseContainer.
// Saved bytes use the server projection; this is only a pending-edit indicator.
function scalar(raw) {
 raw=raw.trim();
 if(raw.startsWith('"')) {
  const match=/^"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$/.exec(raw);
  if(!match)return null;
  let valid=true;
  const value=match[1].replace(/\\(U[\da-fA-F]{8}|u[\da-fA-F]{4}|x[\da-fA-F]{2}|[0-7]{3}|.)/g,(_,escape)=>{
   const simple={a:'\x07',b:'\b',f:'\f',n:'\n',r:'\r',t:'\t',v:'\v','\\':'\\','"':'"'};
   if(Object.hasOwn(simple,escape))return simple[escape];
   let number;
   if(/^[Uux]/.test(escape) && escape.length===({U:9,u:5,x:3})[escape[0]])number=parseInt(escape.slice(1),16);
   else if(/^[0-7]{3}$/.test(escape) && parseInt(escape,8)<=255)number=parseInt(escape,8);
   if(number===undefined || number>0x10ffff || number>=0xd800 && number<=0xdfff){valid=false;return '';}
   return String.fromCodePoint(number);
  });
  return valid?value:null;
 }
 if(raw.startsWith("'")) {
  const match=/^'((?:[^']|'')*)'\s*(?:#.*)?$/.exec(raw);
  return match?match[1].replaceAll("''","'"):null;
 }
 return raw.split(' #',1)[0].trim();
}
export function pendingContainer(content) {
 if(!/^---\r?\n/.test(content))return '';
 // Only the front matter is read, line by line, never the whole note: this
 // runs on every keystroke.
 const encoder=new TextEncoder(),lines=[];let at=0;
 const next=()=>{const end=content.indexOf('\n',at),line=content.slice(at,end<0?content.length:end+1);at+=line.length;return line};
 let offset=encoder.encode(next()).length,end=-1;
 while(at<content.length) {
  const line=next();
  if(line.replace(/[\r\n]+$/,'')==='---'){end=lines.length;break;}
  lines.push(line);
  offset+=encoder.encode(line).length;
  if(offset>65536)break;
 }
 if(end<0)return 'invalid';
 const fields=lines;let value='';
 for(let i=0;i<fields.length;i++) {
  const line=fields[i].replace(/[\r\n]+$/,''),colon=line.indexOf(':');
  if(colon<0 || scalar(line.slice(0,colon))!=='fmn-container')continue;
  if(value || line.slice(0,colon)!=='fmn-container')return 'invalid';
  value=scalar(line.slice(colon+1));
  if(value!=='draft' && value!=='archive')return 'invalid';
  for(const next of fields.slice(i+1)) {
   if(!next.trim() || next.trim().startsWith('#'))continue;
   if(/^[ \t]/.test(next))return 'invalid';
   break;
  }
 }
 return value;
}
