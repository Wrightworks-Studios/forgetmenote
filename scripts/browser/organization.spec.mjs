import {test, expect} from '@playwright/test';
import {readFile} from 'node:fs/promises';
import {startApp} from './app-fixture.mjs';
let app;
const account = {username: 'organization-admin', password: 'DisposableOrganization123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'forgetmenote-organization-', account});
  await app.api('/p/default/api/note', {path: 'Welcome.md', content: '# Team notebook', revision: ''});
});
test.afterAll(async () => app?.close());
async function administration(page) {
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await expect(page.locator('#admin-dialog')).toBeVisible();
}

test('Administration displays the canonical credential-free startup summary', async ({page}) => {
  await administration(page);
  await page.locator('#admin-dialog [data-settings-category=instance]').click();
  const response = await page.request.get(app.origin + '/api/admin');
  expect(response.status()).toBe(200);
  const {instance} = await response.json();
  const rows = await page
    .locator('#instance-status')
    .evaluate(node =>
      Object.fromEntries(
        [...node.querySelectorAll('dt')].map(term => [term.textContent, term.nextElementSibling.textContent])
      )
    );
  for (const [key, label] of [
    ['chat', 'AI conversations'],
    ['decisions', 'AI checks'],
    ['dataConnections', 'Data connections'],
    ['repositorySources', 'Repository sources'],
    ['managedBackups', 'Managed backups'],
    ['automations', 'Automation definitions']
  ])
    expect(rows[label]).toBe(instance[key] ? 'Configured' : 'Not configured');
  expect(rows['Browser interface']).toBe(instance.headless ? 'Disabled' : 'Enabled');
  expect(rows['Outgoing actions']).toBe(instance.outgoingPaused ? 'Paused' : 'Enabled');
  const {modules} = await app.api('/api/organization/action/organization_status', {});
  expect(modules.length).toBeGreaterThan(0);
  expect(rows['Modules on']).toBe(modules.map(module => module.title).join('; '));
  expect(rows['Modules off']).toBe('None');
});

test('groups, invitations and revocation work through real browser sessions', async ({page, browser}) => {
  await administration(page);
  await page.locator('#admin-dialog [data-settings-category=groups]').click();
  await page.locator('#group-id').fill('engineering');
  await page.locator('#group-name').fill('Engineering');
  await page.locator('#group-form button[type=submit], #group-form button:not([type])').click();
  await expect(page.locator('#admin-status')).toHaveText('Group saved.');
  await app.api('/api/organization/action/set_project_group', {project: 'default', group: 'engineering', role: 'read'});
  await page.locator('#admin-dialog [data-settings-category=people]').click();
  await page.locator('#invite-person summary').click();
  await page.locator('#invitation-name').fill('Invited reader');
  await page.locator('#invitation-groups input[value=engineering]').check();
  await page.locator('#invitation-create-form button').click();
  await expect(page.locator('#organization-secret')).toBeVisible();
  const link = await page.locator('#organization-secret-value').inputValue();
  const context = await browser.newContext();
  const recipient = await context.newPage();
  try {
    await recipient.goto(link);
    await expect(recipient.locator('#invitation-form')).toBeVisible();
    await recipient.locator('#invite-username').fill('invited-reader');
    await recipient.locator('#invite-password').fill('DisposableInvited123!');
    await recipient.locator('#invite-confirm').fill('DisposableInvited123!');
    await recipient.locator('#invite-accept').click();
    await expect(recipient.locator('#invite-status')).toContainText('Done');
    await recipient.goto(app.origin + '/login');
    await recipient.locator('#username').fill('invited-reader');
    await recipient.locator('#password').fill('DisposableInvited123!');
    await recipient.locator('form[action="/auth/password/login"] button').click();
    await recipient.goto(app.origin + '/?project=default');
    await expect(recipient.locator('#project-select')).toHaveValue('default');
    expect((await recipient.request.get(app.origin + '/p/default/api/notes')).status()).toBe(200);
    await page.locator('[data-close=admin-dialog]').click();
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await page.locator('#admin-dialog [data-settings-category=groups]').click();
    await page.locator('#group-select').selectOption('engineering');
    await page.locator('#group-members label').filter({hasText: 'Invited reader'}).locator('input').uncheck();
    await page.locator('#group-form button:not([type]), #group-form button[type=submit]').click();
    await expect(page.locator('#admin-status')).toHaveText('Group saved.');
    await expect
      .poll(async () => (await recipient.request.get(app.origin + '/p/default/api/notes')).status())
      .toBe(403);
    await recipient.goto(link);
    await expect(recipient.locator('#invite-status')).not.toBeEmpty();
    await expect(recipient.locator('#invitation-form')).toBeHidden();
  } finally {
    await context.close();
  }
});

test('automation controls issue scoped credentials and mobile administration fits', async ({page}) => {
  await administration(page);
  await page.locator('#admin-dialog [data-settings-category=automation]').click();
  await expect(page.locator('#service-steward')).toContainText(account.username);
  await page.locator('#service-name').fill('Nightly agent');
  await page.locator('#service-create-form button').click();
  await expect(page.locator('#service-details')).toContainText('Nightly agent');
  const row = page.locator('#service-details .person-row').filter({hasText: 'My notes'});
  await row.locator('select').selectOption('edit');
  await row.getByRole('button', {name: 'Save', exact: true}).click();
  const credentials = page.locator('#service-details form').filter({hasText: 'New credential'});
  await expect(credentials.getByLabel('Notebooks', {exact: true}).locator('option[value=default]')).toHaveCount(1);
  await credentials.getByLabel('Name', {exact: true}).fill('Nightly API');
  await credentials.getByLabel('Notebooks', {exact: true}).selectOption('default');
  await credentials.getByRole('button', {name: 'Create credential'}).click();
  await expect(page.locator('#organization-secret-value')).toHaveValue(/^fmn_/);
  const token = await page.locator('#organization-secret-value').inputValue();
  const response = await page.request.post(app.origin + '/api/v1/actions/list_projects', {
    data: {},
    headers: {Authorization: 'Bearer ' + token}
  });
  expect(response.status(), await response.text()).toBe(200);
  await page.locator('#service-details').getByRole('button', {name: 'Disable account', exact: true}).click();
  await expect(page.locator('#service-details')).toContainText('Enable account');
  expect(
    (
      await page.request.post(app.origin + '/api/v1/actions/list_projects', {
        data: {},
        headers: {Authorization: 'Bearer ' + token}
      })
    ).status()
  ).toBe(401);
  await page.setViewportSize({width: 390, height: 844});
  await page.locator('#admin-dialog [data-settings-category=activity]').click();
  await expect(page.locator('#activity-list')).toContainText('service.create');
  expect(await page.locator('#admin-dialog').evaluate(e => e.scrollWidth <= e.clientWidth + 1)).toBe(true);
  await page.locator('#admin-dialog [data-settings-category=instance]').click();
  await page.locator('#instance-name').fill('Example team');
  await page.locator('#instance-form button[type=submit]').click();
  await expect(page.locator('#admin-status')).toHaveText('Branding saved.');
  await expect(page.locator('#instance-people')).toBeHidden();
  await page.screenshot({path: '/tmp/forgetmenote-organization-mobile.png'});
});

test('instance branding updates the header, icons, plugin and mobile form', async ({page}) => {
  await administration(page);
  await page.locator('#admin-dialog [data-settings-category=instance]').click();
  await page.locator('#instance-name').fill('Custom Research');
  const logo = await readFile(new URL('../../internal/branding/assets/apple-touch-icon.png', import.meta.url));
  await page.locator('#instance-logo').setInputFiles({name: 'team.png', mimeType: 'image/png', buffer: logo});
  await expect(page.locator('#instance-logo-preview')).toHaveAttribute('src', /^data:image\/png/);
  await page.locator('#instance-form button[type=submit]').click();
  await expect(page.locator('#admin-status')).toHaveText('Branding saved.');
  await expect(page.locator('.brand span')).toHaveText('Custom Research');
  await expect(page.locator('.brand img')).toHaveAttribute('src', /^\/static\/brand\/instance.png\?v=/);
  await page.setViewportSize({width: 390, height: 844});
  expect(await page.locator('#instance-form').evaluate(e => e.scrollWidth <= e.clientWidth + 1)).toBe(true);
  await page.screenshot({path: '/tmp/forgetmenote-branding-mobile.png'});
  await page.locator('[data-close=admin-dialog]').click();
  await page.reload();
  await expect(page.locator('.brand span')).toHaveText('Custom Research');
  await page.goto(app.origin + '/login');
  await expect(page.locator('.login-brand')).toHaveText('Custom Research');
  await page.goto(app.origin + '/account/connect');
  await expect(page.locator('#mcp-endpoint')).toHaveValue(app.origin + '/mcp');
  await expect(page.locator('link[rel=icon]')).toHaveAttribute('href', /^\/static\/brand\/instance.png/);
  await page.setViewportSize({width: 1200, height: 900});
  await administration(page);
  await page.locator('#admin-dialog [data-settings-category=instance]').click();
  await page.locator('#instance-logo-reset').click();
  await page.locator('#instance-form button[type=submit]').click();
  await expect(page.locator('#admin-status')).toHaveText('Branding saved.');
  await expect(page.locator('.brand img')).toHaveAttribute('src', '/static/brand/flower.svg');
});

test('an Administration status announces a failure as an alert and the next success as a status', async ({page}) => {
  await administration(page);
  await page.locator('#admin-dialog [data-settings-category=instance]').click();
  const branding = '**/api/organization/action/set_instance_branding';
  await page.route(branding, route =>
    route.fulfill({
      status: 409,
      contentType: 'application/json',
      body: JSON.stringify({error: 'Synthetic branding conflict.', code: 'conflict'})
    })
  );
  const save = page.locator('#instance-form button[type=submit]');
  await save.click();
  await expect(page.locator('#admin-status')).toHaveText('Synthetic branding conflict.');
  await expect(page.locator('#admin-status')).toHaveAttribute('role', 'alert');
  await page.unroute(branding);
  await save.click();
  await expect(page.locator('#admin-status')).toHaveText('Branding saved.');
  await expect(page.locator('#admin-status')).toHaveAttribute('role', 'status');
});

test('an Administration action that is still saving ignores another submit', async ({page}) => {
  await administration(page);
  await page.locator('#admin-dialog [data-settings-category=instance]').click();
  const branding = '**/api/organization/action/set_instance_branding';
  let release,
    requests = 0;
  const gate = new Promise(resolve => (release = resolve));
  await page.route(branding, async route => {
    requests++;
    await gate;
    await route.continue().catch(() => {});
  });
  try {
    const save = page.locator('#instance-form button[type=submit]');
    await save.click();
    await expect.poll(() => requests).toBe(1);
    await expect(save).toBeDisabled();
    await page
      .locator('#instance-form')
      .evaluate(
        (form, submitter) => form.dispatchEvent(new SubmitEvent('submit', {submitter, cancelable: true})),
        await save.elementHandle()
      );
    await expect(save).toBeDisabled();
    release();
    await expect(page.locator('#admin-status')).toHaveText('Branding saved.');
    await expect(save).toBeEnabled();
    expect(requests).toBe(1);
  } finally {
    release();
    await page.unroute(branding);
  }
});
