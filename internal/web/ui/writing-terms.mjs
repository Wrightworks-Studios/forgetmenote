export function validateTerms(terms) {
 if(!Array.isArray(terms)||terms.length>32)throw Error('Use at most 32 terms.');
 return terms.map(term=>{
  const name=String(term.name||'').trim(),aliases=[...new Set((term.aliases||[]).map(x=>String(x).trim()))];
  if(!name||aliases.length>8||[name,...aliases].some(x=>!x||[...x].length>100||/[\r\n|]/.test(x)))throw Error('Use a name and up to 8 aliases, each at most 100 characters.');
  return {name,aliases};
 });
}
export function parseTerms(value){return validateTerms(value.split('\n').map(line=>line.replace(/^\s*-\s+/, '').trim()).filter(line=>line&&!line.startsWith('#')).map(line=>{const [name,...aliases]=line.split('|').map(s=>s.trim());return {name,aliases};}));}
export function glossaryMarkdown(terms){return '# Writing glossary\n\n'+validateTerms(terms).map(t=>'- '+[t.name,...t.aliases].join(' | ')).join('\n')+'\n';}
export function parseGlossary(content){
 const body=content.replace(/^---\r?\n[\s\S]*?\r?\n(?:---|\.\.\.)\s*\r?\n/,'').trim();
 if(!/^# Writing glossary\r?\n/.test(body+'\n')||body.split('\n').slice(1).some(line=>line.trim()&&!/^\s*-\s+/.test(line)))throw Error('Choose a Writing glossary note.');
 return parseTerms(body.split('\n').slice(1).join('\n'));
}
