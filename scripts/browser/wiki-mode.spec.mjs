import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
let app;
const account = {username: 'wiki-admin', password: 'DisposableWikiAdmin123!'};
const password = 'DisposableWikiMember123!';
const handbook = {
  'index.md': '---\ntitle: Handbook\n---\nStart with [Getting started](guide/start.md).\n',
  'guide/index.md': '---\ntitle: The guide\norder: 1\n---\nHow the team works.\n',
  'guide/start.md': '---\ntitle: Getting started\norder: 1\n---\nFirst, read [the next steps](next.md).\n',
  'guide/next.md': '---\ntitle: Next steps\norder: 2\n---\nSecond, practise the lighthouse drill.\n',
  'guide/appendix.md': '---\ntitle: Appendix\n---\nA short glossary.\n',
  'Policies.md': '---\norder: 2\n---\n# Policies\nEveryone follows [Getting started](guide/start.md).\n',
  'Glossary.md':
    '---\norder: 3\n---\n# Glossary\nSee [[next STEPS]], [[guide/appendix|the appendix]] and [[Missing page]].\n\n' +
    'Written as `[[Next steps]]`.\n\n```\n[[Policies]]\n```\n'
};
test.beforeAll(async () => {
  app = await startApp({name: 'forgetmenote-wiki-', account});
  await app.api('/api/projects', {id: 'handbook', name: 'Team handbook'});
  for (const [path, content] of Object.entries(handbook))
    await app.api('/p/handbook/api/note', {path, content, revision: ''});
  await app.api('/api/projects', {id: 'plain', name: 'Field notes'});
  for (const path of ['Weather.md', 'birds/Herons.md'])
    await app.api('/p/plain/api/note', {
      path,
      content: '# ' + path.split('/').pop().slice(0, -3) + '\nObserved today.\n',
      revision: ''
    });
  for (const [username, role] of [
    ['wiki-reader', 'read'],
    ['wiki-editor', 'edit']
  ]) {
    const person = await app.api('/api/local-users', {username, password});
    for (const project of ['handbook', 'plain']) await app.api('/api/members', {project, user: person.id, role});
  }
});
test.afterAll(async () => {
  await app?.close();
});
async function signIn(browser, username) {
  const context = await browser.newContext({viewport: {width: 1280, height: 900}});
  const page = await context.newPage();
  page.on('pageerror', error => {
    throw error;
  });
  if (!username) await app.login(page, 'password');
  else {
    const response = await page.request.post(app.origin + '/auth/password/login', {
      form: {username, password},
      headers: {Origin: app.origin}
    });
    expect(response.ok()).toBe(true);
  }
  return page;
}
const topLevel = page => page.locator('#wiki-tree > li > a, #wiki-tree > li > details > summary > a');

test('an owner opens a notebook as a wiki and readers land on its home page', async ({browser}, info) => {
  const owner = await signIn(browser);
  await owner.goto(app.origin + '/?project=handbook');
  await expect(owner.locator('#wiki-toggle')).toHaveText('Read as wiki');
  await expect(owner.locator('body')).not.toHaveClass(/wiki-layout/);
  await owner.locator('#settings-open').click();
  await owner.locator('#settings-dialog [data-settings-category=sharing]').click();
  await expect(owner.locator('#wiki-default')).not.toBeChecked();
  await owner.locator('#wiki-default').check();
  await expect(owner.locator('#wiki-setting-status')).toHaveText('Members now open this notebook as a wiki.');
  await owner.context().close();

  const reader = await signIn(browser, 'wiki-reader');
  await reader.goto(app.origin + '/?project=handbook');
  await expect(reader.locator('body')).toHaveClass(/wiki-layout/);
  await expect(reader.locator('#preview')).toContainText('Start with Getting started');
  await expect(reader).toHaveURL(/layout=wiki/);
  await expect(reader).toHaveURL(/note=index\.md/);
  await expect(topLevel(reader)).toHaveText(['Home', 'The guide', 'Policies', 'Glossary']);
  await expect(reader.locator('#wiki-tree .wiki-home-link a')).toHaveAttribute('aria-current', 'page');
  await expect(reader.locator('#wiki-toggle')).toHaveText('Editor layout');
  for (const hidden of ['#mode', '#browse-root', '#sidebar-new-note', '#search-open', '#note-details-toggle'])
    await expect(reader.locator(hidden)).toBeHidden();

  await reader.locator('#preview a', {hasText: 'Getting started'}).click();
  await expect(reader.locator('#path')).toHaveText('Getting started');
  await expect(reader).toHaveURL(/note=guide%2Fstart\.md/);
  await expect(reader).toHaveURL(/layout=wiki/);
  await expect(reader.locator('#wiki-tree details[open] li a')).toHaveText([
    'Getting started',
    'Next steps',
    'Appendix'
  ]);
  await expect(reader.locator('#wiki-tree a[aria-current=page]')).toHaveText('Getting started');
  await expect(reader.locator('#wiki-pager .wiki-previous')).toHaveText('PreviousThe guide');
  await expect(reader.locator('#wiki-pager .wiki-next')).toHaveText('NextNext steps');
  await expect(reader.locator('#wiki-backlinks-list a')).toHaveCount(2);
  expect((await reader.locator('#wiki-backlinks-list a').allTextContents()).sort()).toEqual(['Handbook', 'Policies']);
  for (const theme of ['light', 'dark']) {
    await reader.evaluate(t => (document.documentElement.dataset.theme = t), theme);
    await reader.screenshot({path: info.outputPath('wiki-page-' + theme + '.png'), fullPage: true});
  }
  await reader.evaluate(() => delete document.documentElement.dataset.theme);

  await reader.locator('#wiki-pager .wiki-next').click();
  await expect(reader.locator('#path')).toHaveText('Next steps');
  // Glossary links here with [[next STEPS]] only.
  await expect(reader.locator('#wiki-backlinks-list a')).toHaveCount(2);
  expect((await reader.locator('#wiki-backlinks-list a').allTextContents()).sort()).toEqual([
    'Getting started',
    'Glossary'
  ]);
  await reader.locator('#wiki-pager .wiki-next').click();
  await expect(reader.locator('#path')).toHaveText('Appendix');
  await expect(reader.locator('#wiki-pager .wiki-next')).toHaveCount(0);
  await expect(reader.locator('#wiki-backlinks-list a')).toHaveText(['Glossary']);
  await reader.locator('#wiki-tree summary a', {hasText: 'The guide'}).click();
  await expect(reader.locator('#preview')).toContainText('How the team works.');
  await expect(reader.locator('#wiki-pager .wiki-previous')).toHaveCount(0);
  await reader.goBack();
  await expect(reader.locator('#path')).toHaveText('Appendix');
  await reader.context().close();
});

test('anyone can switch a wiki notebook back to the editor layout', async ({browser}) => {
  const reader = await signIn(browser, 'wiki-reader');
  await reader.goto(app.origin + '/?project=handbook&note=Policies.md');
  await expect(reader.locator('body')).toHaveClass(/wiki-layout/);
  await expect(reader.locator('#path')).toHaveText('Policies');
  await reader.locator('#wiki-toggle').click();
  await expect(reader.locator('body')).not.toHaveClass(/wiki-layout/);
  await expect(reader.locator('#path')).toHaveText('Policies');
  await expect(reader).toHaveURL(/layout=editor/);
  await expect(reader.locator('#browse-root')).toBeVisible();
  await expect(reader.locator('#wiki-navigation')).toBeHidden();
  await expect(reader.locator('#wiki-footer')).toBeHidden();
  // This browser keeps the choice for the notebook.
  await reader.goto(app.origin + '/?project=handbook');
  await expect(reader.locator('#folder-browser')).toBeVisible();
  await expect(reader.locator('body')).not.toHaveClass(/wiki-layout/);
  await reader.locator('#wiki-toggle').click();
  await expect(reader.locator('body')).toHaveClass(/wiki-layout/);
  await expect(reader.locator('#path')).toHaveText('Handbook');
  await expect(reader.locator('#folder-browser')).toBeHidden();
  await reader.context().close();
});

test('editors open the normal editor from a wiki page', async ({browser}) => {
  const editor = await signIn(browser, 'wiki-editor');
  await editor.goto(app.origin + '/?project=handbook&note=guide%2Fnext.md');
  await expect(editor.locator('body')).toHaveClass(/wiki-layout/);
  await expect(editor.locator('#mode')).toHaveText('Edit page');
  await editor.locator('#mode').click();
  await expect(editor.locator('body')).not.toHaveClass(/wiki-layout/);
  await expect(editor.locator('#editing')).toBeVisible();
  await expect(editor.locator('#mode')).toHaveText('Read');
  await expect(editor).toHaveURL(/layout=editor/);
  await editor.locator('#mode').click();
  await expect(editor.locator('#mode')).toHaveText('Edit');
  await editor.context().close();
});

test('the wiki search box finds pages in its notebook', async ({browser}) => {
  const reader = await signIn(browser, 'wiki-reader');
  await reader.goto(app.origin + '/?project=handbook&layout=wiki');
  await expect(reader.locator('#preview')).toContainText('Start with Getting started');
  await reader.locator('#wiki-query').fill('lighthouse drill');
  await reader.locator('#wiki-query').press('Enter');
  await expect(reader.locator('#wiki-results a')).toHaveText(['Next steps']);
  await expect(reader.locator('#wiki-tree')).toBeHidden();
  await reader.locator('#wiki-results a').click();
  await expect(reader.locator('#path')).toHaveText('Next steps');
  await expect(reader).toHaveURL(/layout=wiki/);
  await reader.locator('#wiki-query').fill('');
  await expect(reader.locator('#wiki-tree')).toBeVisible();
  await expect(reader.locator('#wiki-results')).toBeHidden();
  await reader.context().close();
});

test('[[Page title]] links find pages by title or path and offer to create missing ones', async ({browser}, info) => {
  const reader = await signIn(browser, 'wiki-reader');
  await reader.goto(app.origin + '/?project=handbook&note=Glossary.md');
  await expect(reader.locator('#preview a.wiki-link')).toHaveText(['next STEPS', 'the appendix']);
  await expect(reader.locator('#preview a.wiki-link').first()).toHaveAttribute('href', /note=guide%2Fnext\.md/);
  // Readers cannot create the missing page; code keeps its brackets.
  await expect(reader.locator('#preview span.wiki-link-missing')).toHaveText('Missing page');
  await expect(reader.locator('#preview button.wiki-link-missing')).toHaveCount(0);
  await expect(reader.locator('#preview p code')).toHaveText('[[Next steps]]');
  await expect(reader.locator('#preview pre')).toContainText('[[Policies]]');
  await reader.locator('#preview a.wiki-link', {hasText: 'the appendix'}).click();
  await expect(reader.locator('#path')).toHaveText('Appendix');
  await expect(reader).toHaveURL(/layout=wiki/);
  // The server counts [[…]] links too: the page linked only that way lists its linker.
  await expect(reader.locator('#wiki-backlinks-list a')).toHaveText(['Glossary']);
  await reader.locator('#wiki-backlinks-list a').click();
  await expect(reader.locator('#path')).toHaveText('Glossary');
  await reader.context().close();

  // The links work in the editor layout too, and editors create the missing page.
  const editor = await signIn(browser, 'wiki-editor');
  await editor.goto(app.origin + '/?project=handbook&note=Glossary.md&layout=editor');
  await expect(editor.locator('body')).not.toHaveClass(/wiki-layout/);
  await expect(editor.locator('#preview a.wiki-link').first()).toHaveAttribute('href', /layout=editor/);
  await editor.screenshot({path: info.outputPath('wiki-links.png')});
  await editor.locator('#preview button.wiki-link-missing').click();
  await expect(editor.locator('#new-dialog')).toBeVisible();
  await expect(editor.locator('#new-name')).toHaveValue('Missing page');
  await expect(editor.locator('#new-folder')).toHaveValue('Top level');
  await editor.locator('#new-submit').click();
  await expect(editor.locator('#note-path')).toHaveText('Missing page.md');
  await editor.locator('#save').click();
  await expect(editor.locator('#save-state')).toContainText('Saved');
  await editor.goto(app.origin + '/?project=handbook&note=Glossary.md&layout=editor');
  await expect(editor.locator('#preview a.wiki-link')).toHaveText(['next STEPS', 'the appendix', 'Missing page']);
  await expect(editor.locator('#preview .wiki-link-missing')).toHaveCount(0);
  await editor.context().close();
});

// In the editor (decision 3) a page link shows its label while the caret is
// elsewhere, a link to a missing page is marked as the reader marks it, a
// plain click edits the link and Ctrl/Cmd+click follows it.
test('[[Page title]] links in the editor show labels, mark missing pages and follow on Ctrl/Cmd+click', async ({
  browser
}, info) => {
  const content = '# Index of terms\n\nRead [[next STEPS]], [[guide/appendix|the appendix]] and [[Unwritten page]].\n';
  await app.api('/p/handbook/api/note', {path: 'Terms.md', content, revision: ''});
  const editor = await signIn(browser, 'wiki-editor');
  await editor.goto(app.origin + '/?project=handbook&note=Terms.md&layout=editor');
  await expect(editor.locator('#document')).toHaveAttribute('data-live', 'connected');
  await editor.locator('#mode').click();
  await expect(editor.locator('#editor')).toHaveAttribute('aria-label', 'Note content');
  const links = editor.locator('#editor .cm-lp-wikilink');
  await expect(links).toHaveText(['next STEPS', 'the appendix', 'Unwritten page']);
  await expect(editor.locator('#editor .cm-lp-wikilink-missing')).toHaveText('Unwritten page');
  await expect(editor.locator('#editor .cm-lp-wikilink-missing')).toHaveAttribute(
    'title',
    'No page named “Unwritten page” yet'
  );
  await expect(editor.locator('#editor .cm-lp-wikilink-missing')).toHaveCSS('text-decoration-style', 'dashed');
  await editor.locator('#editor').screenshot({path: info.outputPath('editor-page-links.png')});
  // A plain click edits the link: its Markdown shows.
  await links.filter({hasText: 'the appendix'}).click();
  await expect(editor.locator('#editor .cm-line').filter({hasText: 'Read'})).toContainText(
    '[[guide/appendix|the appendix]]'
  );
  await expect(editor).toHaveURL(/note=Terms\.md/);
  // Ctrl/Cmd+click follows it to the page the reader would open.
  await links.filter({hasText: 'next STEPS'}).click({modifiers: ['ControlOrMeta']});
  await expect(editor).toHaveURL(/note=guide%2Fnext\.md/);
  await expect(editor.locator('#preview')).toContainText('lighthouse drill');
  await editor.goBack();
  await expect(editor).toHaveURL(/note=Terms\.md/);
  await editor.locator('#mode').click();
  // A missing page is offered for creation, as in the reader.
  await editor.locator('#editor .cm-lp-wikilink-missing').click({modifiers: ['ControlOrMeta']});
  await expect(editor.locator('#new-dialog')).toBeVisible();
  await expect(editor.locator('#new-name')).toHaveValue('Unwritten page');
  expect((await app.api('/p/handbook/api/note?path=Terms.md')).content).toBe(content);
  await editor.context().close();
});

test('a notebook without index.md reads as a generated contents page', async ({browser}, info) => {
  const editor = await signIn(browser, 'wiki-editor');
  await editor.goto(app.origin + '/?project=plain');
  await expect(editor.locator('#folder-browser')).toBeVisible();
  await editor.locator('#wiki-toggle').click();
  await expect(editor.locator('#wiki-home')).toBeVisible();
  await expect(editor.locator('#wiki-home-title')).toHaveText('Field notes');
  await expect(editor.locator('#wiki-contents > li > a')).toHaveText(['birds', 'Weather']);
  await expect(editor.locator('#wiki-contents .wiki-folder li a')).toHaveText(['Herons']);
  await expect(editor.locator('#wiki-tree a[aria-current=page]')).toHaveText(['Home']);
  await editor.screenshot({path: info.outputPath('wiki-contents.png')});
  await editor.setViewportSize({width: 390, height: 844});
  await editor.locator('#menu').click();
  await expect(editor.locator('#wiki-tree')).toBeVisible();
  await editor.screenshot({path: info.outputPath('wiki-phone-tree.png')});
  await editor.locator('#wiki-tree a', {hasText: 'Weather'}).click();
  await expect(editor.locator('#path')).toHaveText('Weather');
  await expect(editor.locator('#sidebar')).toBeHidden();
  await expect(editor.locator('#wiki-backlinks-list')).toHaveText('No other pages link here yet.');
  await editor.screenshot({path: info.outputPath('wiki-phone-page.png'), fullPage: true});
  await editor.setViewportSize({width: 1280, height: 900});
  await editor.goBack();
  await expect(editor.locator('#wiki-home-title')).toHaveText('Field notes');
  await editor.locator('#wiki-contents > li > a', {hasText: 'birds'}).click();
  await expect(editor.locator('#wiki-home-title')).toHaveText('birds');
  await expect(editor).toHaveURL(/folder=birds/);
  await expect(editor.locator('#wiki-tree a[aria-current=page]')).toHaveText(['birds']);
  await editor.locator('#wiki-contents a', {hasText: 'Herons'}).click();
  await expect(editor.locator('#path')).toHaveText('Herons');
  await expect(editor.locator('#wiki-home')).toBeHidden();
  await expect(editor.locator('#wiki-pager a')).toHaveCount(0);
  await expect(editor.locator('#wiki-backlinks-list')).toHaveText('No other pages link here yet.');
  await editor.locator('#wiki-tree .wiki-home-link a').click();
  await expect(editor.locator('#wiki-home-title')).toHaveText('Field notes');
  await editor.context().close();
});
