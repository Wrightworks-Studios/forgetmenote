// Frontmatter is source metadata, kept apart from the note's body.
export function splitMarkdown(source) {
  const opening = source.match(/^\ufeff?---[ \t]*\r?\n/);
  if (!opening) return {prefix:'',body:source,error:''};
  const rest = source.slice(opening[0].length);
  const closing = /^(?:---|\.\.\.)[ \t]*(?:\r?\n|$)/m.exec(rest);
  if (!closing) return {prefix:'',body:source,error:'Front matter has no closing delimiter; close it with --- to check this note.'};
  const end = opening[0].length + closing.index + closing[0].length;
  return {prefix:source.slice(0,end),body:source.slice(end),error:''};
}
