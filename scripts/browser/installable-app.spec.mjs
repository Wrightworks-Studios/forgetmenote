import {test, expect, chromium} from '@playwright/test';
import {mkdtemp, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';
import {browserEnvironment} from './no-netlink.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({name: 'fmn-install-', account: {username: 'install-admin', password: 'DisposableInstall123!'}});
});
test.afterAll(async () => app?.close());

test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

// Chromium's own manifest parser and install checks, as its install menu uses them.
async function installability(page) {
  const cdp = await page.context().newCDPSession(page);
  try {
    const manifest = await cdp.send('Page.getAppManifest');
    const {installabilityErrors} = await cdp.send('Page.getInstallabilityErrors');
    return {
      url: manifest.url,
      manifestErrors: manifest.errors,
      installabilityErrors: installabilityErrors.map(error => error.errorId)
    };
  } finally {
    await cdp.detach();
  }
}

test('sign-in and notebook pages offer the instance as an installable app', async () => {
  // The headless shell has no install checks and ordinary contexts are
  // off the record, so this case uses full Chromium with a profile.
  const profile = await mkdtemp(join(tmpdir(), 'fmn-install-profile-'));
  const context = await chromium.launchPersistentContext(profile, {
    channel: 'chromium',
    headless: true,
    executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
    env: browserEnvironment(process.env.FMN_BROWSER_NO_NETLINK)
  });
  try {
    const page = context.pages()[0] || (await context.newPage());
    // A page without a manifest proves the checks run in this browser.
    await page.goto(app.origin + '/healthz');
    expect((await installability(page)).installabilityErrors).toContain('no-manifest');

    const installable = {
      url: app.origin + '/manifest.webmanifest',
      manifestErrors: [],
      installabilityErrors: []
    };
    await page.goto(app.origin + '/login');
    await expect(page.locator('link[rel=manifest]')).toHaveAttribute('href', '/manifest.webmanifest');
    expect(await installability(page)).toEqual(installable);
    await app.login(page, 'password');
    await page.goto(app.origin + '/?project=default&folder=');
    await expect(page.locator('#new-note')).toBeEnabled();
    await expect(page.locator('link[rel=manifest]')).toHaveAttribute('href', '/manifest.webmanifest');
    expect(await installability(page)).toEqual(installable);
  } finally {
    await context.close();
    await rm(profile, {recursive: true, force: true});
  }
});

test('the installed window title bar follows the chosen theme', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.evaluate(() => localStorage.setItem('gitnotes:theme', 'parchment'));
  await page.reload();
  await expect(page.locator('#new-note')).toBeEnabled();
  for (const meta of await page.locator('meta[name=theme-color]').all()) {
    await expect(meta).toHaveAttribute('content', '#f5eedf');
  }
});
