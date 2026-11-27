import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
let app;
const account = {username: 'quicknotes-ui-admin', password: 'DisposableQuicknotesUI123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'forgetmenote-quicknotes-ui-', account});
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await app.login(page, 'password');
});
test('sidebar shows the full Inbox count before opening recent captures', async ({page}) => {
  await page.route('**/api/quicknotes?summary=1', route => route.fulfill({json: {inboxCount: 147}}));
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#count-inbox')).toHaveText('147');
  await expect(page.locator('#inbox-open')).not.toContainText('Private');
  await expect(page.locator('#inbox-view')).toBeHidden();
  await page.setViewportSize({width: 390, height: 844});
  await page.locator('#menu').click();
  await expect(page.locator('#count-inbox')).toBeVisible();
  await expect(page.locator('#count-inbox')).toHaveText('147');
});
test('quick capture is available without selecting a destination or enabling AI', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await page.locator('#create-toggle').click();
  await page.locator('#quicknotes-open').click();
  const dialog = page.getByRole('dialog', {name: 'Quick note', exact: true});
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole('textbox', {name: 'Quick note'})).toBeVisible();
  await expect(dialog.getByRole('button', {name: 'Save to Inbox', exact: true})).toBeVisible();
  await page.setViewportSize({width: 390, height: 844});
  await expect(dialog).toBeInViewport();
  expect(await dialog.evaluate(node => node.scrollWidth <= node.clientWidth)).toBe(true);
  await page.screenshot({path: '/tmp/ui-quicknotes-mobile.png'});
  await dialog.getByRole('button', {name: 'Close', exact: true}).click();
  if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
  await expect(page.locator('#sidebar #notifications-open')).toBeVisible();
  await page.locator('#notifications-open').click();
  await expect(page.getByRole('dialog', {name: 'Notifications', exact: true})).toBeVisible();
  await page
    .getByRole('dialog', {name: 'Notifications', exact: true})
    .getByRole('button', {name: 'Close', exact: true})
    .click();
  await page.locator('#menu').click();
  await expect(page.locator('#main > header #chat-open')).toBeVisible();
});

async function mockQuicknotes(page, {lostAck = false, previousApproval = false} = {}) {
  await page.route('**/api/judgments/settings', route =>
    route.fulfill({
      json: {available: true, localOnly: false, settings: {consent: false, rerank: false, citations: false}}
    })
  );
  const writes = [],
    captures = [];
  let settings = {
    mode: 'suggest',
    cloudConsent: false,
    notifyDestination: true,
    approvedDestinations: previousApproval ? [{project: 'default', folder: 'notes', shared: false}] : [],
    generation: 1
  };
  await page.route('**/api/quicknotes**', async route => {
    const req = route.request(),
      path = new URL(req.url()).pathname,
      body = req.method() === 'POST' ? req.postDataJSON() : null;
    if (body) writes.push({path, body});
    let data;
    if (path.endsWith('/settings')) {
      if (body) settings = {...body, generation: settings.generation + 1};
      data = settings;
    } else if (path.endsWith('/destinations'))
      data = {destinations: [{project: 'default', folder: 'notes', name: 'Shared notebook', shared: true}]};
    else if (path.endsWith('/file')) {
      captures[0] = {
        ...captures[0],
        status: 'filed',
        canUndo: true,
        project: body.destination.project,
        path: 'notes/captured.md'
      };
      data = captures[0];
    } else if (path.endsWith('/undo')) {
      captures[0] = {...captures[0], status: 'inbox', canUndo: false, project: 'private-inbox'};
      data = captures[0];
    } else if (body) {
      data = captures.find(c => c.id === body.operationId);
      if (!data) {
        data = {
          id: body.operationId,
          project: 'private-inbox',
          path: 'capture.md',
          version: 'v1',
          status: 'inbox',
          title: 'Saved private capture',
          excerpt: body.content,
          canUndo: false
        };
        captures.unshift(data);
      }
      if (lostAck) {
        lostAck = false;
        await route.fulfill({status: 502, body: 'Acknowledgement lost'});
        return;
      }
    } else data = {captures, inboxCount: captures.filter(capture => capture.status !== 'filed').length};
    await route.fulfill({json: data});
  });
  return {writes, captures};
}
async function openQuicknotes(page) {
  await page.locator('#create-toggle').click();
  await page.locator('#quicknotes-open').click();
  return page.getByRole('dialog', {name: 'Quick note', exact: true});
}
test('lost capture acknowledgement retains exact text and retry identity across reload', async ({page}) => {
  const {writes, captures} = await mockQuicknotes(page, {lostAck: true});
  await page.goto(app.origin + '/?project=default&folder=');
  let dialog = await openQuicknotes(page);
  await dialog.getByRole('textbox', {name: 'Quick note'}).fill('Private capture survives acknowledgement loss.');
  await dialog.getByRole('button', {name: 'Save to Inbox', exact: true}).click();
  await expect(dialog).toContainText('Acknowledgement lost');
  await expect(dialog.getByRole('textbox', {name: 'Quick note'})).toHaveAttribute('readonly', '');
  await page.reload();
  dialog = await openQuicknotes(page);
  await expect(dialog.getByRole('textbox', {name: 'Quick note'})).toHaveValue(
    'Private capture survives acknowledgement loss.'
  );
  await dialog.getByRole('button', {name: 'Retry save', exact: true}).click();
  await expect(dialog.getByRole('textbox', {name: 'Quick note'})).toHaveValue('');
  const saves = writes.filter(write => write.path === '/api/quicknotes');
  expect(saves).toHaveLength(2);
  expect(saves[1].body).toEqual(saves[0].body);
  expect(captures).toHaveLength(1);
});
test('filing exposes shared scope and uses the saved version; Undo is explicit', async ({page}) => {
  const {writes} = await mockQuicknotes(page);
  await page.goto(app.origin + '/?project=default&folder=');
  let dialog = await openQuicknotes(page);
  await dialog.getByRole('textbox', {name: 'Quick note'}).fill('Capture for filing');
  await dialog.getByRole('button', {name: 'Save to Inbox', exact: true}).click();
  await expect(dialog).toContainText('Saved privately');
  await dialog.getByRole('button', {name: 'Open Inbox', exact: true}).click();
  dialog = page.locator('#inbox-view');
  await expect(dialog).toContainText('Saved private capture');
  await dialog.getByRole('button', {name: 'Choose destination'}).click();
  await dialog.getByLabel('Destination', {exact: true}).selectOption('0');
  await expect(dialog).toContainText('Visible to this notebook’s members.');
  expect(writes.filter(write => write.path.endsWith('/file'))).toHaveLength(0);
  await dialog.getByRole('button', {name: 'File note', exact: true}).click();
  await expect(dialog.getByRole('button', {name: 'Undo filing'})).toBeVisible();
  expect(writes.find(write => write.path.endsWith('/file')).body).toMatchObject({
    version: 'v1',
    destination: {project: 'default', folder: 'notes'}
  });
  await dialog.getByRole('button', {name: 'Undo filing'}).click();
  await expect(dialog).toContainText('Returned to your private Inbox.');
});
test('automatic mode needs explicit remote consent and exact shared destination approval', async ({page}) => {
  const {writes} = await mockQuicknotes(page);
  await page.goto(app.origin + '/?project=default&folder=');
  await page.locator('#inbox-open').click();
  const dialog = page.locator('#inbox-view');
  await dialog.getByRole('button', {name: 'Preferences', exact: true}).click();
  await dialog.getByLabel('Sorting mode').selectOption('automatic');
  await dialog.getByRole('button', {name: 'Save Quicknotes preferences'}).click();
  await expect(dialog).toContainText('requires remote sorting consent');
  await dialog.getByLabel('Allow TypeSafe to read saved Quicknotes for sorting').check();
  await dialog.getByLabel('Shared notebook / notes · shared').check();
  await dialog.getByRole('button', {name: 'Save Quicknotes preferences'}).click();
  await expect(dialog).toContainText('Quicknotes preferences saved.');
  expect(writes.at(-1).body).toEqual({
    mode: 'automatic',
    cloudConsent: true,
    notifyDestination: true,
    approvedDestinations: [{project: 'default', folder: 'notes', shared: true}],
    generation: 1
  });
});
test('actual private capture remains saved after reload without any AI connection', async ({page}) => {
  const before = await (await page.request.get(app.origin + '/api/quicknotes')).json();
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#count-inbox')).toHaveText(String(before.inboxCount));
  let dialog = await openQuicknotes(page);
  const content = '# Browser integration capture\nA private, durable generated note.';
  await dialog.getByRole('textbox', {name: 'Quick note'}).fill(content);
  const saved = page.waitForResponse(
    response => response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/quicknotes'
  );
  await dialog.getByRole('button', {name: 'Save to Inbox', exact: true}).click();
  const response = await saved;
  expect(response.ok(), await response.text()).toBe(true);
  const capture = await response.json();
  expect(capture.status).toBe('inbox');
  expect(capture.version).toBeTruthy();
  await expect(dialog.getByRole('textbox', {name: 'Quick note'})).toHaveValue('');
  await expect(page.locator('#count-inbox')).toHaveText(String(before.inboxCount + 1));
  await page.reload();
  await expect(page.locator('#count-inbox')).toHaveText(String(before.inboxCount + 1));
  await page.locator('#inbox-open').click();
  dialog = page.locator('#inbox-view');
  const card = dialog.locator('[data-capture="' + capture.id + '"]');
  await expect(card).toContainText('Browser integration capture');
  const note = await page.request.get(
    app.origin + '/p/' + encodeURIComponent(capture.project) + '/api/note?path=' + encodeURIComponent(capture.path)
  );
  expect(note.ok()).toBe(true);
  expect((await note.json()).content).toBe(content);
  await card.getByRole('button', {name: 'Choose destination', exact: true}).click();
  await dialog.getByLabel('Destination', {exact: true}).selectOption('0');
  await dialog.getByRole('button', {name: 'File note', exact: true}).click();
  await expect(card.getByRole('button', {name: 'Undo filing', exact: true})).toBeVisible();
  await expect(page.locator('#count-inbox')).toHaveText(String(before.inboxCount));
  await card.getByRole('button', {name: 'Undo filing', exact: true}).click();
  await expect(page.locator('#count-inbox')).toHaveText(String(before.inboxCount + 1));
});

test('an automatic approval made while private does not preapprove a now shared notebook', async ({page}) => {
  await mockQuicknotes(page, {previousApproval: true});
  await page.goto(app.origin + '/?project=default&folder=');
  await page.locator('#inbox-open').click();
  const dialog = page.locator('#inbox-view');
  await dialog.getByRole('button', {name: 'Preferences', exact: true}).click();
  await expect(dialog.getByLabel('Shared notebook / notes · shared')).not.toBeChecked();
});
test('actual shared destination approval preserves the reviewed sharing status', async ({page}) => {
  const reader = await app.api('/api/local-users', {
    username: 'quicknotes-shared-reader',
    password: 'DisposableSharedReader123!'
  });
  await app.api('/api/members', {project: 'default', user: reader.id, role: 'read'});
  await page.goto(app.origin + '/?project=default&folder=');
  await page.locator('#inbox-open').click();
  const dialog = page.locator('#inbox-view');
  await dialog.getByRole('button', {name: 'Preferences', exact: true}).click();
  await dialog.getByLabel('Sorting mode').selectOption('manual');
  const choice = dialog
    .locator('fieldset')
    .getByRole('checkbox', {name: /shared/})
    .first();
  await choice.check();
  const saved = page.waitForResponse(
    response =>
      response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/quicknotes/settings'
  );
  await dialog.getByRole('button', {name: 'Save Quicknotes preferences'}).click();
  const response = await saved;
  expect(response.ok(), await response.text()).toBe(true);
  expect((await response.json()).approvedDestinations).toContainEqual(
    expect.objectContaining({project: 'default', shared: true})
  );
  await expect(dialog).toContainText('Quicknotes preferences saved.');
});
