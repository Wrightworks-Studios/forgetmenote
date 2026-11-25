import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {writeFile} from 'node:fs/promises';
import {join} from 'node:path';
import {origin, dir, api, login, clickNoteAction, useEditorApp} from './editor-fixture.mjs';
useEditorApp();

const secret = () => 'sk-live-' + Math.random().toString(16).slice(2) + Math.random().toString(16).slice(2);

async function openNoteMenu(page, path) {
  await page.goto(origin + '/?project=default&folder=');
  await expect(page.locator('#folder-browser')).toBeVisible();
  await page
    .locator('#folder-items, #folder-groups')
    .getByRole('button', {name: 'Actions for ' + path, exact: true})
    .first()
    .click();
  await expect(page.locator('[data-file-action="purge"]')).toBeEnabled();
}

async function confirmPlan(page, reason) {
  await expect(page.locator('#purge-confirm')).toBeDisabled();
  await page.locator('#purge-reason').fill(reason);
  await expect(page.locator('#purge-confirm')).toBeDisabled();
  await page.locator('#purge-understood').check();
  await expect(page.locator('#purge-confirm')).toBeEnabled();
  // A purge takes as long as it takes under load: its answer, not a fixed
  // timeout, says it finished.
  const executed = page.waitForResponse(
    response =>
      response.request().method() === 'POST' && new URL(response.url()).pathname.endsWith('/api/purge/execute')
  );
  await page.locator('#purge-confirm').click();
  const response = await executed;
  expect(response.status(), await response.text()).toBe(200);
  await expect(page.locator('#purge-done')).toBeVisible();
}

test('an Owner reviews a text purge plan, includes the other occurrence and erases it from every version', async ({
  page
}) => {
  const key = secret(),
    path = 'purge-keys.md',
    other = 'purge-other.md';
  const first = await api('/p/default/api/note', {
    path,
    content: '# Keys\n\nThe staging key is ' + key + '.\n',
    revision: ''
  });
  await api('/p/default/api/note', {
    path,
    content: '# Keys\n\nThe staging key is ' + key + ', rotated soon.\n',
    revision: first.revision
  });
  await api('/p/default/api/note', {path: other, content: '# Other\n\nAlso ' + key + '.\n', revision: ''});
  await login(page);
  await openNoteMenu(page, path);
  await page.locator('[data-file-action="purge"]').click();
  await expect(page.locator('#purge-dialog')).toBeVisible();
  await page.getByLabel('Text in chosen notes, from every version').check();
  await expect(page.locator('#purge-paths')).toHaveValue(path);
  await page.locator('#purge-text').fill(key);
  await page.locator('#purge-review').click();
  await expect(page.locator('#purge-plan')).toBeVisible();
  await expect(page.locator('#purge-what')).toContainText('Replace the exact text');
  await expect(page.locator('#purge-what')).toContainText(key);
  await expect(page.locator('#purge-prepared')).toBeHidden();
  await expect(page.locator('#purge-summary')).toContainText(path);
  await expect(page.locator('#purge-summary')).toContainText('2 versions');
  await expect(page.locator('#purge-elsewhere')).toContainText(other);
  await expect(page.locator('.purge-exposure')).toContainText('rotate a leaked secret regardless');
  await expect(page.locator('.purge-exposure')).toContainText('GitHub may keep cached views');
  await expect(page.locator('.purge-exposure')).toContainText('earlier backups');
  await page.locator('#purge-include').click();
  await expect(page.locator('#purge-summary')).toContainText(other);
  await expect(page.locator('#purge-elsewhere-field')).toBeHidden();
  await confirmPlan(page, 'Leaked staging key');
  await expect(page.locator('#purge-receipt')).toContainText('purge-');
  await expect(page.locator('#purge-receipt')).toContainText('Rotate any leaked secret now');
  await page.locator('#purge-finish').click();
  await expect(page.locator('#purge-dialog')).toBeHidden();
  for (const note of [path, other]) {
    const now = await api('/p/default/api/note?path=' + note);
    expect(now.content).not.toContain(key);
    expect(now.content).toContain('[purged]');
  }
  // A history pin to a rewritten revision reads its replacement.
  const pinned = await api('/p/default/api/note?path=' + path + '&revision=' + first.revision);
  expect(pinned.revision).not.toBe(first.revision);
  expect(pinned.content).toBe('# Keys\n\nThe staging key is [purged].\n');
  const receipts = await api('/p/default/api/purge');
  expect(receipts.receipts[0].reason).toBe('Leaked staging key');
  expect(JSON.stringify(receipts)).not.toContain(key);
});

test('an Owner erases chosen versions from the history dialog and a whole note from its menu', async ({page}) => {
  const path = 'purge-versions.md';
  const first = await api('/p/default/api/note', {
    path,
    content: '# Draft\n\nCustomer card 4242 4242.\n',
    revision: ''
  });
  const second = await api('/p/default/api/note', {
    path,
    content: '# Draft\n\nNo customer data.\n',
    revision: first.revision
  });
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#note-path')).toHaveText(path);
  await clickNoteAction(page, 'history-open');
  await expect(page.locator('#history-purge')).toBeVisible();
  await page.locator('#history-purge').click();
  await expect(page.locator('#purge-dialog')).toBeVisible();
  await expect(page.getByLabel('Chosen versions of this note')).toBeChecked();
  await page
    .locator('#purge-versions label')
    .filter({hasText: first.revision.slice(0, 7)})
    .locator('input')
    .check();
  await page.locator('#purge-review').click();
  await expect(page.locator('#purge-what')).toContainText('Erase 1 chosen version of ' + path);
  await expect(page.locator('#purge-what')).toContainText(first.revision.slice(0, 7));
  await expect(page.locator('#purge-summary')).toContainText('1 version');
  await confirmPlan(page, 'Customer data in an early draft');
  await page.locator('#purge-finish').click();
  const old = await api('/p/default/api/note?path=' + path + '&revision=' + first.revision).catch(error => error);
  expect(String(old.message)).toContain('HTTP 410');
  expect((await api('/p/default/api/note?path=' + path)).content).toBe('# Draft\n\nNo customer data.\n');

  await openNoteMenu(page, path);
  await page.locator('[data-file-action="purge"]').click();
  await expect(page.getByLabel('This whole note, from every version')).toBeChecked();
  await page.locator('#purge-review').click();
  await expect(page.locator('#purge-what')).toContainText('Erase the whole note ' + path);
  await expect(page.locator('#purge-summary')).toContainText(path);
  await confirmPlan(page, 'The note held customer data');
  await page.locator('#purge-finish').click();
  const gone = await api('/p/default/api/note?path=' + path).catch(error => error);
  expect(String(gone.message)).toContain('HTTP 404');
  const pin = await api('/p/default/api/note?path=' + path + '&revision=' + second.revision).catch(error => error);
  expect(String(pin.message)).toContain('HTTP 410');
});

test('an agent prepares a purge and a person confirms it from the link', async ({page}) => {
  const key = secret(),
    path = 'purge-agent.md';
  await api('/p/default/api/note', {path, content: '# Agent found\n\n' + key + '\n', revision: ''});
  const prepared = await api('/api/v1/actions/prepare_purge', {project: 'default', kind: 'text', text: key});
  expect(prepared.plan.confirmation).toBeFalsy();
  expect(JSON.stringify(prepared)).not.toContain(key);
  expect((await api('/p/default/api/note?path=' + path)).content).toContain(key);
  await login(page);
  await page.goto(prepared.confirmUrl);
  await expect(page.locator('#purge-plan')).toBeVisible();
  // The person sees that an agent prepared it, and what it erases.
  await expect(page.locator('#purge-prepared')).toContainText('An agent prepared this plan');
  await expect(page.locator('#purge-what')).toContainText('Replace the exact text');
  await expect(page.locator('#purge-what')).toContainText(key);
  await expect(page.locator('#purge-what')).toContainText('everywhere in this notebook');
  await expect(page.locator('#purge-summary')).toContainText(path);
  await confirmPlan(page, 'An agent found a leaked key');
  expect((await api('/p/default/api/note?path=' + path)).content).not.toContain(key);
  await page.locator('#purge-finish').click();
  // The link, which stood for the text, is spent.
  await page.goto(prepared.confirmUrl);
  await expect(page.locator('#purge-error')).toContainText('expired or was already used');
});

test('a plan an agent prepared to erase a whole note says so before anyone confirms it', async ({page}) => {
  const path = 'purge-agent-note.md';
  await api('/p/default/api/note', {path, content: '# Important\n\nKeep this.\n', revision: ''});
  const prepared = await api('/api/v1/actions/prepare_purge', {project: 'default', kind: 'note', path});
  await login(page);
  await page.goto(prepared.confirmUrl);
  await expect(page.locator('#purge-plan')).toBeVisible();
  await expect(page.locator('#purge-prepared')).toContainText('An agent prepared this plan');
  await expect(page.locator('#purge-what')).toContainText('Erase the whole note ' + path);
  await page.locator('#purge-close').click();
  expect((await api('/p/default/api/note?path=' + path)).content).toContain('Keep this.');
});

test('a plan warns before the purge runs, and the notebook lists its earlier purges', async ({page}) => {
  const key = secret(),
    path = 'purge-warned.md';
  await api('/p/default/api/note', {path, content: '# Warned\n\n' + key + '\n', revision: ''});
  // The previous release's files for the notebook are still in the data
  // directory, which no purge reaches.
  await writeFile(join(dir, 'data', 'projects', 'default', 'write-journal.json'), '{}');
  await login(page);
  await openNoteMenu(page, path);
  await page.locator('[data-file-action="purge"]').click();
  await expect(page.locator('#purge-dialog')).toBeVisible();
  await expect(page.locator('#purge-history')).toBeHidden();
  await page.getByLabel('Text in chosen notes, from every version').check();
  await page.locator('#purge-text').fill(key);
  await page.locator('#purge-review').click();
  await expect(page.locator('#purge-plan')).toBeVisible();
  await expect(page.locator('#purge-warnings')).toBeVisible();
  await expect(page.locator('#purge-warnings')).toContainText("The previous release's files for this notebook");
  await expect(page.locator('#purge-summary')).toContainText('1 version');
  await confirmPlan(page, 'Leaked key in a warned note');
  await page.locator('#purge-finish').click();

  await openNoteMenu(page, path);
  await page.locator('[data-file-action="purge"]').click();
  await expect(page.locator('#purge-history')).toBeVisible();
  await page.locator('#purge-history summary').click();
  await expect(page.locator('#purge-receipts')).toContainText('erased text in chosen notes');
  await expect(page.locator('#purge-receipts')).toContainText('Leaked key in a warned note');
  await expect(page.locator('#purge-receipts')).not.toContainText(key);
  await page.locator('#purge-close').click();
});

test('the list of earlier purges shows unfinished and interrupted purges with what they did not do', async ({page}) => {
  const path = 'purge-listed.md';
  await api('/p/default/api/note', {path, content: '# Listed\n', revision: ''});
  const receipt = {
    notebook: 'default',
    actor: 'browser-admin',
    created: '2026-11-20T10:00:00Z',
    kind: 'text',
    wholeNotebook: true
  };
  await page.route(
    url => url.pathname === '/p/default/api/purge',
    route =>
      route.request().method() === 'GET'
        ? route.fulfill({
            json: {
              receipts: [
                {
                  ...receipt,
                  purge: 'purge-a',
                  reason: 'Still running',
                  unfinished: true,
                  warnings: ['It may not have redacted the records.']
                },
                {
                  ...receipt,
                  purge: 'purge-b',
                  reason: 'Gateway stopped',
                  interrupted: true,
                  warnings: ['A gateway that started since reset the indexes.']
                },
                {...receipt, purge: 'purge-c', reason: 'Leaked key'}
              ]
            }
          })
        : route.continue()
  );
  await login(page);
  await openNoteMenu(page, path);
  await page.locator('[data-file-action="purge"]').click();
  // A purge that did not finish opens the list.
  await expect(page.locator('#purge-receipts')).toBeVisible();
  await expect(page.locator('#purge-receipts > li')).toHaveCount(3);
  await expect(page.locator('#purge-receipts > li').nth(0)).toContainText('Unfinished: erased text everywhere');
  await expect(page.locator('#purge-receipts > li').nth(0)).toContainText('It may not have redacted the records.');
  await expect(page.locator('#purge-receipts > li').nth(1)).toContainText('Interrupted: erased text everywhere');
  await expect(page.locator('#purge-receipts > li').nth(1)).toContainText(
    'A gateway that started since reset the indexes.'
  );
  await expect(page.locator('#purge-receipts > li').nth(2)).toContainText('Leaked key');
  await expect(page.locator('#purge-receipts > li').nth(2)).not.toContainText('Interrupted');
});
