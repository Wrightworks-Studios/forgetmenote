import {test, expect} from '@playwright/test';
import {
  app,
  origin,
  api,
  login,
  openRetainedPrivate,
  clickNoteAction,
  clickNewNote,
  ensureSearch,
  useEditorApp
} from './editor-fixture.mjs';
test.describe.configure({mode: 'parallel'});
useEditorApp();

// Wherever people write a note's or a comment's text, the browser changes
// nothing they type, as in the editor: no autocorrect, auto-capitalization or
// writing suggestions. Spellcheck only underlines.
const asTyped = {autocorrect: 'off', autocapitalize: 'off', writingsuggestions: 'false', spellcheck: 'true'};
async function expectAsTyped(field) {
  for (const [name, value] of Object.entries(asTyped)) await expect(field).toHaveAttribute(name, value);
}

test('the reconcile dialog keeps the combined version as typed', async ({page}) => {
  await openRetainedPrivate(page, 'typed/reconcile.md');
  await page.locator('#draft-compare').click();
  await expect(page.locator('#reconcile-dialog')).toBeVisible();
  await expectAsTyped(page.locator('#reconcile-content'));
  await expectAsTyped(page.locator('#reconcile-latest'));
});

test('retained drafts and reconciliation text show as typed', async ({page}) => {
  const path = 'typed/drafts.md',
    note = await api('/p/default/api/note', {path, content: '# Shared\n', revision: ''}),
    user = await api('/api/me');
  await login(page);
  await page.goto(origin + '/login');
  // Two other tabs retained drafts, one with reconciliation text, so opening
  // the note asks which to open.
  await page.evaluate(
    ({scope, note}) => {
      for (const [owner, content, scratch] of [
        ['11111111-1111-4111-8111-111111111111', '# Draft of one tab\n', '# Combined in one tab\n'],
        ['22222222-2222-4222-8222-222222222222', '# Draft of another tab\n', null]
      ]) {
        localStorage.setItem(
          'gitnotes:draft:@' + owner + '/' + scope,
          JSON.stringify({content, revision: note.revision, version: note.version, savedAt: Date.now()})
        );
        if (scratch) localStorage.setItem('gitnotes:reconcile:@' + owner + '/' + scope, scratch);
      }
    },
    {scope: user.id + '/default/' + path, note}
  );
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#draft-candidates-dialog')).toBeVisible();
  const fields = page.locator('#draft-candidates-list textarea');
  await expect(fields).toHaveCount(3);
  for (const field of await fields.all()) await expectAsTyped(field);
});

test('comments and replies are written as typed', async ({page}) => {
  const path = 'typed/comments.md',
    note = await api('/p/default/api/note', {path, content: '# Harbour\n\nA bright star.\n', revision: ''});
  await api('/p/default/api/comments', {path, text: 'Which star?', revision: note.revision});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#note-details-toggle').click();
  await page.locator('#comments-open').click();
  await expect(page.locator('#comment-label')).toHaveText('Add a comment');
  await expectAsTyped(page.locator('#comment-text'));
  await page.locator('#comments-list').getByRole('button', {name: 'Reply', exact: true}).click();
  await expect(page.locator('#comment-label')).toHaveText(/^Reply to /);
  await expect(page.locator('#comment-text')).toBeFocused();
  await expectAsTyped(page.locator('#comment-text'));
});

test("a note's properties and glossary terms are written as typed", async ({page}) => {
  // Properties writes the title, kind, status and tags into the note's front
  // matter as typed; Save glossary writes the terms into a glossary note.
  const path = 'typed/properties.md';
  await api('/p/default/api/note', {path, content: '# Properties\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await clickNoteAction(page, 'properties-open');
  const properties = page.locator('#properties-dialog');
  await expect(properties).toBeVisible();
  for (const label of ['Display title (optional)', 'Kind', 'Status', 'Tags · one per line'])
    await expectAsTyped(properties.getByLabel(label, {exact: true}));
  await properties.locator('[data-close=properties-dialog]').click();
  // Writing tools are in the editor's toolbar.
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await page.locator('#writing-open').click();
  await page.locator('#writing-tab-terms').click();
  await expect(page.locator('#writing-term-input')).toBeVisible();
  await expectAsTyped(page.locator('#writing-term-input'));
});

test("a note's name and an archived source's title are written as typed", async ({page}) => {
  const path = 'typed/name.md';
  await api('/p/default/api/note', {path, content: '# Name\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await clickNoteAction(page, 'filename-open');
  await expect(page.locator('#filename-input')).toBeVisible();
  await expectAsTyped(page.locator('#filename-input'));
  await page.locator('#filename-cancel').click();
  await ensureSearch(page);
  await page.getByRole('button', {name: 'Actions for ' + path, exact: true}).click();
  await page.locator('[data-file-action=rename]').click();
  await expect(page.locator('#file-name')).toBeVisible();
  await expectAsTyped(page.locator('#file-name'));
  await page.locator('#file-action-dialog [data-close=file-action-dialog]').click();
  await page.locator('#search-close').click();
  await clickNewNote(page);
  await expect(page.locator('#new-name')).toBeVisible();
  await expectAsTyped(page.locator('#new-name'));
  // The title of an archived source becomes its note's title; it shows once
  // a preview is ready.
  await page.getByRole('tab', {name: 'Import source', exact: true}).click();
  await expectAsTyped(page.locator('#archive-title'));
});

test('a quick note and the notebook agent instructions are written as typed', async ({page}) => {
  // Quick notes are for accounts, not the recovery login.
  await app.login(page, 'password');
  await page.goto(origin + '/?project=default&folder=');
  await page.locator('#create-toggle').click();
  await page.locator('#quicknotes-open').click();
  const quick = page.getByRole('dialog', {name: 'Quick note', exact: true});
  await expect(quick).toBeVisible();
  await expectAsTyped(quick.getByRole('textbox', {name: 'Quick note'}));
  await quick.getByRole('button', {name: 'Close', exact: true}).click();
  await page.locator('#settings-open').click();
  await page.locator('#settings-dialog [data-settings-category=instructions]').click();
  await page.locator('#instructions-open').click();
  await expectAsTyped(page.locator('#instructions-text'));
});
