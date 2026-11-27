import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app, cookies;
const account = {username: 'judgments-ui-admin', password: 'DisposableJudgmentsUI123!'};
test.beforeAll(async ({browser}) => {
  app = await startApp({name: 'forgetmenote-judgments-ui-', account});
  await app.api('/api/projects', {id: 'other', name: 'Other synthetic notebook'});
  for (const path of ['Source.md', 'Destination.md'])
    await app.api('/p/default/api/note', {
      path,
      content: '# ' + path + '\nSynthetic committed evidence.\n',
      revision: ''
    });
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await app.login(page, 'password');
    cookies = await context.cookies();
  } finally {
    await context.close();
  }
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await page.context().addCookies(cookies);
});
test('global notifications open from an empty notebook on desktop and mobile', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  for (const width of [1440, 390]) {
    await page.setViewportSize({width, height: 844});
    if (width <= 720) {
      await expect(page.locator('#sidebar')).toBeHidden();
      await page.locator('#menu').click();
      await expect(page.locator('#sidebar')).toBeVisible();
    }
    const bell = page.getByRole('button', {name: 'Notifications', exact: true});
    await expect(bell).toBeVisible();
    await bell.click();
    const dialog = page.getByRole('dialog', {name: 'Notifications', exact: true});
    await expect(dialog).toBeVisible();
    await expect(dialog).toBeInViewport();
    await dialog.getByRole('button', {name: 'Close', exact: true}).click();
    await expect(bell).toBeFocused();
  }
});

async function mockJudgments(page, {localOnly = false, consent = false, notifications = []} = {}) {
  const writes = [];
  let preferences = {consent, rerank: false, citations: false},
    latestJob = {id: 'job1', kind: 'consistency', state: 'paused', examined: 2, total: 4, remaining: 2, results: []};
  await page.route('**/api/judgments/**', async route => {
    const req = route.request(),
      url = new URL(req.url()),
      body = req.method() === 'POST' ? req.postDataJSON() : null;
    if (body) writes.push({path: url.pathname, body});
    let result;
    if (url.pathname === '/api/judgments/settings') {
      if (body) preferences = body;
      result = {available: true, localOnly, settings: preferences};
    } else if (url.pathname.endsWith('/settings'))
      result = {available: true, canManage: true, settings: {autoChecks: body?.autoChecks || false, enabledBy: ''}};
    else if (url.pathname.endsWith('/jobs')) {
      if (body) latestJob = {...latestJob, kind: body.kind};
      result = body ? latestJob : {jobs: []};
    } else result = latestJob;
    await route.fulfill({json: result});
  });
  await page.route('**/api/notifications**', async route => {
    const req = route.request();
    if (req.method() === 'POST') {
      writes.push({path: new URL(req.url()).pathname, body: req.postDataJSON()});
      for (const item of notifications) item.read = true;
    }
    await route.fulfill({json: {unreadCount: notifications.filter(item => !item.read).length, items: notifications}});
  });
  return writes;
}
async function openPreferences(page) {
  await page.locator('#account-actions-toggle').click();
  await page.getByRole('button', {name: 'Your settings', exact: true}).click();
  if (!(await page.locator('#judgments-settings-open').isVisible()))
    await page.locator('#account-dialog [data-settings-category=connections]').click();
  await page.locator('#judgments-settings-open').click();
  return page.getByRole('dialog', {name: 'Evidence checks', exact: true});
}
async function openChecks(page) {
  await page.locator('#review-open').click();
  await page.locator('#judgments-review-open').click();
  return page.getByRole('dialog', {name: 'Review', exact: true});
}
test('personal AI status is read-only and reports local-only policy', async ({page}) => {
  const writes = await mockJudgments(page, {consent: true});
  await page.goto(app.origin + '/?project=default&folder=');
  const settings = await openPreferences(page);
  await expect(settings).toContainText('Administration');
  await expect(settings.locator('input')).toHaveCount(0);
  await expect(settings.getByRole('button', {name: 'Save preferences'})).toHaveCount(0);
  await page.unroute('**/api/judgments/**');
  await mockJudgments(page, {localOnly: true, consent: true});
  await settings.getByRole('button', {name: 'Close', exact: true}).click();
  await page.locator('#judgments-settings-open').click();
  await expect(settings).toContainText('local-only policy');
  expect(writes).toEqual([]);
});
test('manual notebook checks use explicit operations and preserve partial progress', async ({page}) => {
  const writes = await mockJudgments(page, {consent: true});
  await page.goto(app.origin + '/?project=default&folder=');
  const dialog = await openChecks(page);
  await expect(dialog.getByLabel('Check committed changes automatically')).toHaveCount(0);
  await expect(dialog).toContainText('Automatic checks: Disabled');
  await dialog.getByRole('button', {name: 'Check now', exact: true}).click();
  await expect(dialog).toContainText('2 of 4 examined');
  await expect(dialog.getByRole('button', {name: 'Continue check'})).toBeVisible();
  expect(writes[0].body.kind).toBe('consistency');
  expect(writes[0].body.operationId).toMatch(/^[a-f0-9-]{36}$/);
  expect(writes[0].body).not.toHaveProperty('budgetTokens');
  await dialog.getByRole('button', {name: 'Continue check'}).click();
  expect(writes.some(write => write.path.endsWith('/jobs/job1/continue') && Object.keys(write.body).length === 0)).toBe(
    true
  );
});
test('notifications show paired excerpts and personal read actions without injecting source HTML', async ({page}) => {
  const revision = 'a'.repeat(40),
    writes = await mockJudgments(page, {
      notifications: [
        {
          id: 'notice1',
          project: 'default',
          job: 'job1',
          kind: 'consistency',
          title: 'Different counts',
          summary: 'Inspect the two statements.',
          read: false,
          evidence: [
            {path: 'first.md', revision, text: '<img src=x onerror=alert(1)> Count 4.'},
            {path: 'second.md', revision, text: 'Count 5.'}
          ]
        }
      ]
    });
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#notifications-count')).toHaveText('1');
  await page.locator('#notifications-open').click();
  const dialog = page.getByRole('dialog', {name: 'Notifications', exact: true});
  await expect(dialog.locator('blockquote')).toHaveCount(2);
  await expect(dialog.locator('img')).toHaveCount(0);
  await expect(dialog.getByRole('link', {name: 'Inspect · aaaaaaa'})).toHaveCount(2);
  await page.setViewportSize({width: 390, height: 844});
  expect(await dialog.evaluate(node => node.scrollWidth <= node.clientWidth)).toBe(true);
  await page.screenshot({path: '/tmp/ui-notifications-mobile.png'});
  await dialog.getByRole('button', {name: 'Mark read'}).click();
  await expect(page.locator('#notifications-count')).toBeHidden();
  expect(writes.at(-1).body).toEqual({ids: ['notice1']});
});
test('audit source revisions and destination choices are explicit before work starts', async ({page}) => {
  const writes = await mockJudgments(page, {consent: true});
  await page.goto(app.origin + '/?project=default&folder=');
  const dialog = await openChecks(page);
  await dialog.getByRole('button', {name: 'Choose sources and destinations'}).click();
  await dialog.getByRole('button', {name: 'Start audit', exact: true}).click();
  await expect(dialog).toContainText('Select at least one source note.');
  expect(writes).toHaveLength(0);
  await dialog.getByRole('button', {name: 'Select · Source.md', exact: true}).click();
  await dialog.getByLabel('Destination.md', {exact: true}).check();
  await dialog.getByRole('button', {name: 'Start audit', exact: true}).click();
  await expect(dialog).toContainText('audit · paused');
  expect(writes[0].body.sources).toEqual([{path: 'Source.md', revision: expect.stringMatching(/^[a-f0-9]{40}$/)}]);
  expect(writes[0].body.destinations).toEqual(['Destination.md']);
});
test('correction actions create an unsent chat draft containing selected evidence only', async ({page}) => {
  const revision = 'a'.repeat(40);
  await mockJudgments(page, {
    notifications: [
      {
        id: 'correction',
        project: 'default',
        title: 'Inspect counts',
        summary: 'Two counts differ.',
        evidence: [{path: 'Source.md', revision, text: 'The count was four.'}],
        read: true
      }
    ]
  });
  let turns = 0;
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().endsWith('/turns')) turns++;
  });
  await page.goto(app.origin + '/?project=default&folder=');
  await page.locator('#notifications-open').click();
  await page
    .getByRole('dialog', {name: 'Notifications', exact: true})
    .getByRole('button', {name: 'Ask chat to propose'})
    .click();
  await expect(page.locator('#chat-input')).toHaveValue(/The count was four/);
  await expect(page.locator('#chat-input')).toHaveValue(/do not apply/);
  expect(turns).toBe(0);
});
test('a lost job acknowledgement retries the same operation instead of creating another', async ({page}) => {
  await mockJudgments(page, {consent: true});
  const attempts = [];
  await page.route('**/p/default/api/judgments/jobs/retry-job', route =>
    route.fulfill({
      json: {id: 'retry-job', kind: 'consistency', state: 'completed', examined: 1, total: 1, remaining: 0, results: []}
    })
  );
  await page.route('**/p/default/api/judgments/jobs', async route => {
    if (route.request().method() === 'GET') {
      await route.fulfill({json: {jobs: []}});
      return;
    }
    const body = route.request().postDataJSON();
    attempts.push(body);
    if (attempts.length === 1) await route.fulfill({status: 502, body: 'Job acknowledgement lost'});
    else
      await route.fulfill({
        json: {
          id: 'retry-job',
          kind: 'consistency',
          state: 'completed',
          examined: 1,
          total: 1,
          remaining: 0,
          results: []
        }
      });
  });
  await page.goto(app.origin + '/?project=default&folder=');
  const dialog = await openChecks(page);
  await dialog.getByRole('button', {name: 'Check now', exact: true}).click();
  await expect(dialog).toContainText('Job acknowledgement lost');
  await dialog.getByRole('button', {name: 'Check now', exact: true}).click();
  await expect(dialog).toContainText('consistency · completed');
  expect(attempts).toHaveLength(2);
  expect(attempts[1]).toEqual(attempts[0]);
});
test('late notebook job responses cannot cross navigation scope', async ({page}) => {
  await mockJudgments(page, {consent: true});
  let release, completed;
  const gate = new Promise(resolve => (release = resolve)),
    finished = new Promise(resolve => (completed = resolve));
  await page.route('**/p/default/api/judgments/jobs', async route => {
    await gate;
    try {
      await route.fulfill({json: {jobs: [{id: 'old', kind: 'PRIVATE_OLD_NOTEBOOK_RESULT', state: 'completed'}]}});
    } catch {
    } finally {
      completed();
    }
  });
  await page.goto(app.origin + '/?project=default&folder=');
  const dialog = await openChecks(page);
  await expect(dialog).toContainText('Loading…');
  await dialog.getByRole('button', {name: 'Close', exact: true}).click();
  await page.locator('#project-select').selectOption('other');
  await openChecks(page);
  await expect(dialog).toContainText('No checks yet.');
  release();
  await finished;
  await expect(dialog).not.toContainText('PRIVATE_OLD_NOTEBOOK_RESULT');
});

test('notebook review status does not expose personal configuration controls', async ({page}) => {
  const writes = await mockJudgments(page);
  await page.route('**/p/default/api/judgments/settings', route =>
    route.fulfill({json: {available: false, canManage: false, managed: true, settings: {autoChecks: true}}})
  );
  await page.goto(app.origin + '/?project=default&folder=');
  const dialog = await openChecks(page);
  await expect(dialog).toContainText('Automatic checks: Enabled');
  await expect(dialog.getByRole('button', {name: 'Save automatic checks'})).toHaveCount(0);
  await expect(dialog.getByRole('button', {name: 'Check now', exact: true})).toBeDisabled();
  expect(writes).toEqual([]);
});
