import {test} from 'node:test';
import assert from 'node:assert/strict';
import {splitMarkdown} from './markdown-model.mjs';

test('frontmatter is retained verbatim outside visual editing', () => {
  const header = '\ufeff---\r\ntitle: "Original"\r\nunknown: [a, b]\r\n---\r\n';
  const source = header + '# Body\r\n';
  assert.deepEqual(splitMarkdown(source), {prefix:header, body:'# Body\r\n', error:''});
  // Every mode edits the front matter's text, so the advice is to close it.
  assert.equal(splitMarkdown('---\nnot closed').error,'Front matter has no closing delimiter; close it with --- to check this note.');
  assert.equal(splitMarkdown('# Text\n---\n').body,'# Text\n---\n');
});
