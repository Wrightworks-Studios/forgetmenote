import {test, expect} from '@playwright/test';
import {startApp, withClipboard} from './app-fixture.mjs';
let app, employee;
const account = {username: 'connect-admin', password: 'DisposableConnect123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'forgetmenote-connect-', account});
  employee = await app.api('/api/local-users', {username: 'employee', password: 'DisposableEmployee123!'});
  await app.api('/api/organization/action/set_instance_name', {name: 'Research & Design'});
  await app.api('/api/organization/action/set_group', {id: 'employees', name: 'Employees', members: [employee.id]});
  await app.api('/api/organization/action/set_project_group', {project: 'default', group: 'employees', role: 'read'});
});
test.afterAll(async () => app?.close());
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app.log', {body: app.output, contentType: 'text/plain'});
});

test('administrator provisions an ordinary work notebook; employee setup and downloads fit desktop and mobile', async ({
  page,
  browser
}, info) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#account-actions-toggle').click();
  await expect(page.getByRole('link', {name: 'Connect your AI', exact: true})).toBeVisible();
  await page.locator('#admin-open').click();
  await page.locator('#admin-dialog [data-settings-category=people]').click();
  await page.locator('#access-list').getByRole('button').filter({hasText: 'employee'}).click();
  await page.locator('#work-notebook-provision summary').click();
  await page.locator('#work-notebook-form').getByLabel('Notebook name', {exact: true}).fill('My work');
  await page.locator('#work-notebook-form').getByLabel('Notebook ID', {exact: true}).fill('employee-work');
  await page.locator('#work-notebook-form button').click();
  await expect(page.locator('#admin-status')).toContainText('Work notebook ready');
  const context = await browser.newContext({permissions: ['clipboard-read', 'clipboard-write']});
  const user = await context.newPage();
  try {
    const login = await user.request.post(app.origin + '/auth/password/login', {
      form: {username: 'employee', password: 'DisposableEmployee123!'},
      headers: {Origin: app.origin}
    });
    expect(login.status(), await login.text()).toBe(200);
    await user.goto(app.origin + '/account/connect');
    await expect(user.getByRole('heading', {name: 'Connect your AI'})).toBeVisible();
    await expect(user.locator('.eyebrow')).toHaveText('Research & Design');
    await expect(user.locator('.connect-notebooks li').filter({hasText: 'My work'})).toContainText(
      'Readable and writable'
    );
    await expect(user.locator('.connect-notebooks li').filter({hasText: 'My notes'})).toContainText('Read-only');
    await withClipboard(async () => {
      await user.getByRole('button', {name: 'Copy endpoint'}).click();
      await expect(user.locator('#connect-status')).toHaveText('Copied.');
      expect(await user.evaluate(() => navigator.clipboard.readText())).toBe(app.origin + '/mcp');
    });
    await user.getByLabel('Your AI client').focus();
    await user.keyboard.press('o');
    await user.keyboard.press('Tab');
    await expect(user.locator('[data-client=codex]')).toBeVisible();
    await expect(user.getByRole('link', {name: 'Download Research & Design for Codex'})).toBeVisible();
    await expect(user.getByRole('button', {name: 'Copy installation prompt'})).toBeVisible({timeout: 3000});
    const installPrompt = await user.getByLabel('Installation prompt').inputValue();
    expect(installPrompt).toContain('"Research & Design"');
    expect(installPrompt).toContain('research_design_mcp');
    expect(installPrompt).toContain(app.origin + '/mcp');
    expect(installPrompt).toContain('install.py install');
    expect(installPrompt).toContain('If Python 3 is unavailable');
    await withClipboard(async () => {
      await user.getByRole('button', {name: 'Copy installation prompt'}).click();
      await expect(user.locator('#connect-status')).toHaveText('Copied.');
      expect(await user.evaluate(() => navigator.clipboard.readText())).toBe(installPrompt);
    });
    for (const client of ['codex', 'claude']) {
      await user.getByLabel('Your AI client').selectOption(client);
      const downloadPromise = user.waitForEvent('download');
      await user.locator('[data-client=' + client + '] [data-download]').click();
      const download = await downloadPromise;
      expect(download.suggestedFilename()).toMatch(new RegExp('forgetmenote-[a-f0-9]{16}-' + client + '-0.3.4.zip'));
      await download.saveAs(info.outputPath(client + '.zip'));
    }
    await user.route('**/account/connect/download/claude', route =>
      route.fulfill({status: 503, contentType: 'text/plain', body: 'Package temporarily unavailable'})
    );
    await user.locator('[data-client=claude] [data-download]').click();
    await expect(user.getByRole('status')).toContainText('HTTP 503');
    await expect(user.getByRole('status')).toContainText('Package temporarily unavailable');
    await user.unroute('**/account/connect/download/claude');
    await user.getByLabel('Your AI client').selectOption('codex');
    await user.screenshot({path: info.outputPath('connect-desktop.png'), fullPage: true});
    await user.setViewportSize({width: 390, height: 844});
    for (const client of ['chatgpt', 'codex', 'claude', 'other']) {
      await user.getByLabel('Your AI client').selectOption(client);
      expect(await user.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    }
    await user.getByLabel('Your AI client').selectOption('codex');
    await user.screenshot({path: info.outputPath('connect-mobile.png'), fullPage: true});
    await user.evaluate(() => localStorage.setItem('gitnotes:theme', 'dark'));
    await user.reload();
    await expect(user.locator('html')).toHaveAttribute('data-theme', 'dark');
    await user.getByLabel('Your AI client').selectOption('claude');
    await user.screenshot({path: info.outputPath('connect-mobile-dark.png'), fullPage: true});
    await user.getByRole('link', {name: 'Manage sign-in and revoke sessions'}).click();
    await expect(user.locator('#account-dialog')).toBeVisible();
    await expect(user.getByRole('button', {name: 'Revoke your browser and AI sessions'})).toBeVisible();
    const forbidden = await user.request.post(app.origin + '/api/organization/action/provision_work_notebook', {
      data: {id: 'forbidden', name: 'No', user: employee.id},
      headers: {Origin: app.origin}
    });
    expect(forbidden.status(), await forbidden.text()).toBe(403);
  } finally {
    await context.close();
  }
});

test('headless setup retains downloads and functional session revocation', async ({page}) => {
  const headless = await startApp({name: 'forgetmenote-headless-connect-', account, env: {HEADLESS: 'true'}});
  try {
    await headless.login(page, 'password');
    await page.goto(headless.origin + '/account/connect');
    await expect(page.getByRole('heading', {name: 'Connect your AI'})).toBeVisible();
    await page.getByRole('button', {name: 'Revoke your browser and AI sessions'}).click();
    await expect(page).toHaveURL(/\/login$/);
    const response = await page.request.get(headless.origin + '/account/connect/download/codex');
    expect(response.status(), await response.text()).toBe(401);
  } finally {
    await headless.close();
  }
});
