import {test} from 'node:test';
import assert from 'node:assert/strict';
import {noteOutline} from './note-outline.mjs';

const outline=markdown=>{
  const {title,headings}=noteOutline(markdown);
  return {title:title && [title.level,title.text,title.from],headings:headings.map(heading=>[heading.level,heading.text,heading.from])};
};

test('the leading level 1 heading after front matter is the title, the others are sections',()=>{
  const markdown='---\ntitle: Book\n# not a heading\n---\n\n# Book\n\n## One\n\nText.\n\n### One, part\n';
  assert.deepEqual(outline(markdown),{
    title:[1,'Book',markdown.indexOf('# Book')],
    headings:[[2,'One',markdown.indexOf('## One')],[3,'One, part',markdown.indexOf('### One')]],
  });
  // A title only when it is the first block, as the reader's first element.
  assert.deepEqual(outline('Intro.\n\n# Later\n\n## Part\n').title,null);
  assert.deepEqual(outline('Intro.\n\n# Later\n\n## Part\n').headings.map(heading=>heading[1]),['Later','Part']);
  assert.deepEqual(outline('## First\n\n# Second\n').title,null);
});

test('front matter is only what the server leaves out of the reading view',()=>{
  // Not closed within the note, or not opened by exactly ---: Markdown.
  assert.deepEqual(outline('---\n# Open\n\n## Part\n').headings.map(heading=>heading[1]),['Open','Part']);
  assert.deepEqual(outline('--- \ntitle: x\n---\n# Title\n').headings.map(heading=>heading[1]),['title: x','Title']);
  const crlf='---\r\ntitle: x\r\n---\r\n# Title\r\n\r\n## Part\r\n';
  assert.deepEqual(outline(crlf),{title:[1,'Title',crlf.indexOf('# Title')],headings:[[2,'Part',crlf.indexOf('## Part')]]});
});

test('ATX and setext headings, but not code, HTML blocks, quotes or lists',()=>{
  const markdown=[
    '# Title',
    '',
    '##   Closed  ##',
    '#hashtag is text',
    '####### seven is text',
    '\\# escaped',
    '````',
    '# in a fence',
    '```',
    '````',
    '~~~ js',
    '## in a tilde fence',
    '~~~',
    '',
    '    # indented code',
    '',
    'Setext *one*',
    'over two lines',
    '===',
    '',
    'Setext two',
    '---',
    '',
    '---',
    '',
    '<!--',
    '# in a comment',
    '-->',
    '<div>',
    '# in a block',
    '',
    '> # quoted',
    '> ## still quoted',
    '> text',
    'lazy line',
    '---',
    '',
    '- item',
    '',
    '  # in the item',
    '',
    '## After the list',
    '#',
    '### ###',
    '',
  ].join('\n');
  assert.deepEqual(outline(markdown).headings.map(([level,text])=>[level,text]),[
    [2,'Closed'],[1,'Setext one over two lines'],[2,'Setext two'],[2,'After the list'],[1,''],[3,''],
  ]);
  assert.equal(outline(markdown).title[1],'Title');
  // An unclosed fence runs to the end of the note.
  assert.deepEqual(outline('# T\n\n## A\n\n```\n## B\n').headings.map(heading=>heading[1]),['A']);
  // A thematic break after a blank line is no setext underline; a paragraph
  // continues over an indented line and a list that cannot interrupt it.
  assert.deepEqual(outline('Text\n\n---\n').headings,[]);
  assert.deepEqual(outline('Para\n    more\n2. not a list\n---\n').headings.map(heading=>heading[1]),['Para more 2. not a list']);
});

// As in CommonMark (and the server's goldmark), only paragraph text continues
// a list item or a block quote lazily: a heading, fence, thematic break or
// HTML block that is not indented to the item's content ends it.
test('a heading right after a list or a block quote is a section',()=>{
  const names=markdown=>outline(markdown).headings.map(heading=>heading[1]);
  const compact='# Notes\n## Groceries\n- milk\n## Todo\n- x\n## Ideas\ntext\n';
  assert.deepEqual(outline(compact),{
    title:[1,'Notes',0],
    headings:[[2,'Groceries',compact.indexOf('## Groceries')],[2,'Todo',compact.indexOf('## Todo')],[2,'Ideas',compact.indexOf('## Ideas')]],
  });
  assert.deepEqual(names('# T\n\n1. step\n2. step\n### Result\nok\n\n## Later\n'),['Result','Later']);
  assert.deepEqual(names('> q\n## Next\n'),['Next']);
  assert.deepEqual(names('- a\nlazy text\n## After a lazy line\n'),['After a lazy line']);
  // Not indented to the item's content: 10. puts it at the fourth column.
  assert.deepEqual(names('10. ten\n   ## Three spaces\n'),['Three spaces']);
  assert.deepEqual(names('- a\n\n ## One space after a blank\n'),['One space after a blank']);
  // A blank line ends a block quote.
  assert.deepEqual(names('> q\n\n  ## Indented after a quote\n'),['Indented after a quote']);
  // A fence or an HTML block ends the list too, and holds its own lines.
  assert.deepEqual(names('- a\n```\n## in code\n```\n## After the fence\n'),['After the fence']);
  assert.deepEqual(names('- a\n<!--\n## in a comment\n-->\n## After the comment\n'),['After the comment']);
  assert.deepEqual(names('- a\n***\nSetext after a break\n===\n'),['Setext after a break']);
  // Indented to the content, or quoted, the heading is in the item or quote.
  assert.deepEqual(names('- a\n  ## nested\n'),[]);
  assert.deepEqual(names('1. a\n   ## nested\n'),[]);
  assert.deepEqual(names('- a\n\t## tab\n'),[]);
  assert.deepEqual(names('- a\n  - b\n  ## in the first item\n'),[]);
  assert.deepEqual(names('-     code\n  ## in the item\n'),[]);
  assert.deepEqual(names('> q\n> ## quoted\n>\n> ## still quoted\n'),[]);
  assert.deepEqual(names('> q\n- item\n  ## in the item\n'),[]);
  // A setext underline is no lazy line: === stays text in the quote.
  assert.deepEqual(names('> quote\nlazy\n===\n'),[]);
  // Only a paragraph continues: after a quoted heading, a line is the note's.
  assert.deepEqual(names('> ## quoted\nnot lazy\n---\n'),['not lazy']);
  assert.deepEqual(names('-     code\ntext\n ## After code\n'),['After code']);
  assert.deepEqual(names('- a\n\n  b\nlazy\n===\n'),[]);
  assert.deepEqual(names('- a\n  ```\n  code\ntext\n  ## After the item\n'),['After the item']);
  // An item can start with one blank line, not two.
  assert.deepEqual(names('-\n  ## in the item\n'),[]);
  assert.deepEqual(names('-\n\n  ## After an empty item\n'),['After an empty item']);
  // Compact notes still have Contents: the title and a section.
  assert.deepEqual(outline('# Notes\n- a\n## Todo\n- b\n'),{title:[1,'Notes',0],headings:[[2,'Todo',12]]});
});

// The server leaves HTML out of the reading view, so a heading after an
// HTML block is still its first element: the title.
test('a level 1 heading after HTML is the title',()=>{
  const markdown='<!-- draft -->\n# Title\n\n## Part\n';
  assert.deepEqual(outline(markdown),{title:[1,'Title',markdown.indexOf('# Title')],headings:[[2,'Part',markdown.indexOf('## Part')]]});
  assert.deepEqual(outline('<div>\nnotes\n</div>\n\n# Title\n').title?.[1],'Title');
  assert.deepEqual(outline('Text\n<!-- c -->\n\n# Later\n').title,null);
  // A line that only starts with a tag is a paragraph, which the reader shows.
  assert.deepEqual(outline('<span>x</span> text\n\n# Later\n').title,null);
  assert.deepEqual(outline('</pre>\n# Later\n').headings.map(heading=>heading[1]),['Later']);
});

// HTML blocks as goldmark finds them: block tags interrupt a paragraph, and
// a line of one other tag starts a block only outside a paragraph.
test('HTML blocks hold their lines up to a blank line or their end',()=>{
  const names=markdown=>outline(markdown).headings.map(heading=>heading[1]);
  assert.deepEqual(names('Para\n<div>\n# in a block\n\n# After\n'),['After']);
  assert.deepEqual(names('Para\n<span>\n# heading\n'),['heading']);
  assert.deepEqual(names('<span class="x">\n# in a block\n\n## After\n'),['After']);
  assert.deepEqual(names('<span>x</span>\n# heading\n'),['heading']);
  assert.deepEqual(names('- a\n<span>\n## lazy\n'),['lazy']);
  assert.deepEqual(names('- a\n<div>\n## in a block\n'),[]);
});

// A paragraph that holds a table (GFM, as goldmark finds one) is no
// setext heading's text: the underline starts a block of its own.
test('a table followed by an underline is no heading',()=>{
  const names=markdown=>outline(markdown).headings.map(heading=>heading[1]);
  assert.deepEqual(names('| a | b |\n|---|---|\n| 1 | 2 |\n---\n## After\n'),['After']);
  assert.deepEqual(names('Text\n--- | ---\n===\n'),[]);
  assert.deepEqual(names('Name\n:---\n---\n'),[]);
  assert.deepEqual(names('Intro\na | b\n- | -\n-\n  ## in the item\n'),[]);
  // Lines before the table's header row stay a paragraph: the heading's text.
  const before='Intro\nmore\n| a | b |\n|---|---|\n---\n';
  assert.deepEqual(outline(before).headings,[[2,'Intro more',0]]);
  // More header cells than columns, or no delimiter row: a heading.
  assert.deepEqual(names('a | b | c\n--- | ---\n---\n'),['a | b | c --- | ---']);
  assert.deepEqual(names('Plain | text\n---\n'),['Plain | text']);
});

test('heading text is plain, as the reader shows it',()=>{
  const texts=noteOutline([
    '# Title',
    '## The *quiet* **harbour** and _its_ ~~old~~ ships',
    '## A [link](https://example.com/a_(b)) and ![an image](x.png)',
    '## [[Page]] and [[Other page|a label]]',
    '## Code `a *b* c` and snake_case_name',
    '## Escaped \\*stars\\* and <em>tags</em>',
    '## Runs ``x`y`` and ``` a`b',
  ].join('\n')).headings.map(heading=>heading.text);
  assert.deepEqual(texts,[
    'The quiet harbour and its old ships',
    'A link and an image',
    'Page and a label',
    'Code a *b* c and snake_case_name',
    'Escaped *stars* and tags',
    'Runs x`y and ``` a`b',
  ]);
});
