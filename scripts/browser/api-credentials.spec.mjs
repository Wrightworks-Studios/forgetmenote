import {test, expect} from '@playwright/test';
import {startApp, withClipboard, watchConfirmations} from './app-fixture.mjs';

let app, member;
const account = {username: 'credentials-admin', password: 'DisposableCredentials123!'};
const memberAccount = {username: 'credentials-member', password: 'DisposableMember123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'fmn-api-credentials-', account});
  member = await app.api('/api/local-users', memberAccount);
  await app.api('/api/members', {project: 'default', user: member.id, role: 'edit'});
});
test.afterAll(async () => app?.close());
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

async function signIn(context, credentials) {
  const response = await context.request.post(app.origin + '/auth/password/login', {
    form: credentials,
    headers: {Origin: app.origin}
  });
  expect(response.ok(), await response.text()).toBe(true);
}
const listProjects = (page, token) =>
  page.request.post(app.origin + '/api/v1/actions/list_projects', {
    data: {},
    headers: {Authorization: 'Bearer ' + token}
  });

test('a person creates, copies and revokes their own API credential, and its secret is shown once', async ({
  browser
}, testInfo) => {
  const context = await browser.newContext({permissions: ['clipboard-read', 'clipboard-write']});
  try {
    await signIn(context, memberAccount);
    const page = await context.newPage();
    const logged = [];
    page.on('console', message => logged.push(message.text()));
    await page.goto(app.origin);
    await page.locator('#account-actions-toggle').click();
    await page.locator('#account-open').click();
    await page.locator('#account-dialog [data-settings-category=credentials]').click();
    const panel = page.locator('#account-dialog [data-settings-panel=credentials]');
    await expect(panel.getByRole('heading', {name: 'Your API credentials', exact: true})).toBeVisible();
    await expect(page.locator('#credential-list')).toHaveText('No API credentials.');

    await panel.locator('#credential-create summary').click();
    await panel.getByLabel('Name', {exact: true}).fill('Laptop script');
    await panel.getByLabel('Notebooks', {exact: true}).selectOption('default');
    await panel.getByLabel('Permission', {exact: true}).selectOption({label: 'Read and write'});
    await panel.getByLabel('Use with', {exact: true}).selectOption({label: 'API only'});
    await panel.getByLabel('Expires in days', {exact: true}).fill('14');
    await panel.getByRole('button', {name: 'Create credential', exact: true}).click();

    const secret = page.getByRole('dialog', {name: 'Copy your new credential', exact: true});
    await expect(secret).toBeVisible();
    await expect(secret).toContainText('only time');
    const field = secret.getByLabel('Secret', {exact: true});
    await expect(field).toHaveValue(/^fmn_/);
    const token = await field.inputValue();
    await withClipboard(async () => {
      await secret.getByRole('button', {name: 'Copy', exact: true}).click();
      await expect(secret.getByRole('status')).toHaveText('Copied.');
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(token);
    });
    // The credential is the person's, limited to what the form chose.
    expect((await listProjects(page, token)).status()).toBe(200);
    const mcp = await page.request.post(app.origin + '/mcp', {
      data: {jsonrpc: '2.0', id: 1, method: 'tools/list'},
      headers: {Authorization: 'Bearer ' + token, Accept: 'application/json, text/event-stream'}
    });
    expect(mcp.status()).toBe(401);

    await secret.screenshot({path: testInfo.outputPath('credential-secret.png')});
    await secret.getByRole('button', {name: 'Done', exact: true}).click();
    await expect(secret).toBeHidden();
    // Closed, the secret is gone from the page and was never kept or logged.
    await expect(page.locator('#credential-secret-value')).toHaveCount(0);
    expect(await page.content()).not.toContain(token);
    const stored = await page.evaluate(() => JSON.stringify({...localStorage}) + JSON.stringify({...sessionStorage}));
    expect(stored).not.toContain(token);
    expect(logged.filter(text => text.includes(token))).toEqual([]);

    const rows = page.locator('#credential-list .person-row');
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText('Laptop script');
    await expect(rows.first()).toContainText('Read and write');
    await expect(rows.first()).toContainText('My notes');
    await expect(rows.first()).toContainText('API only');

    // A second credential for every notebook, dismissed with Escape.
    await panel.getByLabel('Name', {exact: true}).fill('Every notebook');
    await panel.getByLabel('All your notebooks, including ones shared with you later', {exact: true}).check();
    await expect(panel.getByLabel('Notebooks', {exact: true})).toBeDisabled();
    await panel.getByRole('button', {name: 'Create credential', exact: true}).click();
    await expect(secret).toBeVisible();
    const wide = await secret.getByLabel('Secret', {exact: true}).inputValue();
    await page.keyboard.press('Escape');
    await expect(secret).toBeHidden();
    await expect(page.locator('#credential-secret-value')).toHaveCount(0);
    await expect(rows).toHaveCount(2);
    await expect(rows.filter({hasText: 'Every notebook'})).toContainText('All notebooks');
    await page.setViewportSize({width: 390, height: 844});
    expect(await panel.evaluate(e => e.scrollWidth <= e.clientWidth + 1)).toBe(true);
    await page.screenshot({path: testInfo.outputPath('credentials-mobile.png'), fullPage: true});
    await page.setViewportSize({width: 1280, height: 720});

    // A reload returns to this page, whose list holds the credentials, never a secret.
    await page.reload();
    await expect(page.locator('#account-dialog [data-settings-category=credentials]')).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    await expect(rows).toHaveCount(2);
    for (const value of [token, wide]) expect(await page.content()).not.toContain(value);

    await watchConfirmations(page, dialog => dialog.accept(), true);
    await rows.filter({hasText: 'Laptop script'}).getByRole('button', {name: 'Revoke', exact: true}).click();
    await expect(page.locator('#credential-status')).toHaveText('Credential revoked.');
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText('Every notebook');
    expect((await listProjects(page, token)).status()).toBe(401);
    expect((await listProjects(page, wide)).status()).toBe(200);
  } finally {
    await context.close();
  }
});

test('an Admin revokes one credential of a person from People and leaves the others working', async ({
  browser,
  page
}) => {
  const context = await browser.newContext();
  let first, second;
  try {
    await signIn(context, memberAccount);
    const create = async name => {
      const response = await context.request.post(app.origin + '/integrations/actions/create_api_credential', {
        data: {name, projects: ['default'], scopes: ['notes:read'], audiences: ['api'], days: 3},
        headers: {Origin: app.origin}
      });
      expect(response.ok(), await response.text()).toBe(true);
      return (await response.json()).token;
    };
    first = await create('Nightly export');
    second = await create('Reading app');
  } finally {
    await context.close();
  }
  await app.login(page, 'password');
  await page.goto(app.origin);
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await page.locator('#access-list').getByRole('button', {name: memberAccount.username}).click();
  const rows = page.locator('#user-offboarding .person-row');
  const revoked = rows.filter({hasText: 'Nightly export'}),
    kept = rows.filter({hasText: 'Reading app'});
  await expect(kept).toHaveCount(1);
  await revoked.getByRole('button', {name: 'Revoke', exact: true}).click();
  await expect(revoked).toHaveCount(0);
  await expect(kept).toHaveCount(1);
  expect((await listProjects(page, first)).status()).toBe(401);
  expect((await listProjects(page, second)).status()).toBe(200);
});
