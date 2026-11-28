import {test, expect} from '@playwright/test';
import {mkdtemp, rm, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';

// Managed backups with Google Drive and Dropbox configured and no GitHub App.
// The app reaches neither provider here: the authorization page is stubbed,
// and the steps after a provider's callback are served from page routes.
let app, secrets;
const account = {username: 'drive-admin', password: 'DisposableDrives123!'};
const path = 'ForgetMeNote/default-abc123';
test.beforeAll(async () => {
  secrets = await mkdtemp(join(tmpdir(), 'forgetmenote-drive-secrets-'));
  const file = async (name, content) => {
    await writeFile(join(secrets, name), content, {mode: 0o600});
    return join(secrets, name);
  };
  const config = {
    workers: {fixture: {owner: 'local', tokenFile: await file('worker', 'w'.repeat(40))}},
    drives: {
      sealKeyFile: await file('seal', 'ab'.repeat(32) + '\n'),
      google: {clientId: 'drive-test.example.com', clientSecretFile: await file('google', 'google-secret')},
      dropbox: {appKey: 'dropbox-test-key', appSecretFile: await file('dropbox', 'dropbox-secret')}
    }
  };
  app = await startApp({
    name: 'forgetmenote-drive-backups-',
    account,
    env: {BACKUP_APP_CONFIG_FILE: await file('backups.json', JSON.stringify(config))}
  });
  await app.api('/api/projects', {id: 'alpha', name: 'Alpha notes'});
});
test.afterAll(async () => {
  await app?.close();
  if (secrets) await rm(secrets, {recursive: true, force: true});
});

async function openBackups(page, query = '') {
  await page.goto(app.origin + '/?project=default' + query);
  if (!query) {
    await page.locator('#settings-open').click();
    await page.locator('#settings-dialog [data-settings-category=history]').click();
  }
  await expect(page.locator('#managed-backup')).toBeVisible();
}

test('the drive rows connect through the provider with PKCE and only the destination scope', async ({page}) => {
  await app.login(page, 'password');
  let authorization;
  await page.route('https://accounts.google.com/**', route => {
    authorization = new URL(route.request().url());
    return route.fulfill({status: 200, contentType: 'text/plain', body: 'Provider stub'});
  });
  await openBackups(page);
  // No GitHub App: the GitHub row goes, each configured drive kind has one.
  await expect(page.locator('#managed-backup-github')).toBeHidden();
  const google = page.locator('#drive-backup-gdrive');
  await expect(google).toContainText('Google Drive backup');
  await expect(google).toContainText('Not connected.');
  await expect(page.locator('#drive-backup-dropbox')).toContainText('Dropbox backup');
  await google.getByRole('button', {name: 'Connect Google Drive'}).click();
  await expect(page.locator('body')).toHaveText('Provider stub');
  const query = authorization.searchParams;
  expect(query.get('scope')).toBe('https://www.googleapis.com/auth/drive.file');
  expect(query.get('access_type')).toBe('offline');
  expect(query.get('prompt')).toBe('consent');
  expect(query.get('code_challenge_method')).toBe('S256');
  expect(query.get('code_challenge')).toMatch(/^[A-Za-z0-9_-]{43}$/);
  expect(query.get('redirect_uri')).toBe(app.origin + '/backup/google/callback');
  expect(query.get('client_id')).toBe('drive-test.example.com');
  expect(query.get('state')).toBeTruthy();
});

test('the setup screen names the account and folder, needs consent, and shows the password once', async ({page}) => {
  await app.login(page, 'password');
  let drive = {kind: 'gdrive', project: 'default', name: 'Google Drive', connected: false, generation: 0};
  const controls = [];
  let enabled;
  await page.route('**/p/default/api/managed-backup', async route => {
    const response = await route.fetch();
    const status = await response.json();
    status.drives = status.drives.map(entry => (entry.kind === 'gdrive' ? drive : entry));
    return route.fulfill({response, json: status});
  });
  await page.route('**/p/default/api/managed-backup/drive/pending?kind=gdrive', route =>
    route.fulfill({
      json: {
        kind: 'gdrive',
        name: 'Google Drive',
        account: 'owner@example.com',
        path,
        continuing: false,
        encrypted: false
      }
    })
  );
  await page.route('**/p/default/api/managed-backup/drive/enable', route => {
    enabled = route.request().postDataJSON();
    drive = {
      ...drive,
      connected: true,
      enabled: true,
      encrypted: true,
      generation: 1,
      account: 'owner@example.com',
      path,
      lastRevision: 'c'.repeat(40),
      lastSnapshot: 'default-20261127T100000Z-cccccccccccc.bundle.enc',
      lastSuccess: '2026-11-27T10:00:00Z',
      kept: 2
    };
    return route.fulfill({json: {drive, password: 'p'.repeat(43)}});
  });
  await page.route('**/p/default/api/managed-backup/control', route => {
    controls.push(route.request().postDataJSON());
    return route.fulfill({json: drive});
  });
  await openBackups(page, '&backup=gdrive');
  const google = page.locator('#drive-backup-gdrive');
  await expect(google).toContainText(`folder ${path} in the Google Drive account owner@example.com`);
  await google.getByRole('checkbox', {name: /Encrypt snapshots/}).check();
  await google.getByRole('button', {name: 'Enable backup'}).click();
  // The browser's own validation keeps the setup until the Owner consents.
  expect(enabled).toBeUndefined();
  await google.getByRole('checkbox', {name: /Anyone with access to this folder/}).check();
  await google.getByRole('button', {name: 'Enable backup'}).click();
  await expect(google.getByRole('textbox', {name: 'Google Drive snapshot password'})).toHaveValue('p'.repeat(43));
  expect(enabled).toEqual({kind: 'gdrive', generation: 0, encrypt: true, consent: true});
  await expect(google).toContainText('It is shown only this once');
  await expect(page).not.toHaveURL(/backup=gdrive/);
  await expect(google.getByRole('status')).toHaveText(`owner@example.com · ${path} · Encrypted · Up to date`);
  await expect(google).toContainText('Verified cccccccccccc');
  await expect(google).toContainText('2 snapshots kept');
  await google.getByRole('button', {name: 'I saved the password'}).click();
  await expect(google.getByRole('textbox', {name: 'Google Drive snapshot password'})).toBeHidden();
  await google.getByRole('button', {name: 'Back up now'}).click();
  await expect.poll(() => controls.length).toBe(1);
  expect(controls[0]).toEqual({kind: 'gdrive', generation: 1, action: 'now'});
  await google.getByRole('button', {name: 'Disconnect'}).click();
  await expect(page.locator('dialog[data-confirm-action]')).toContainText(
    'Snapshots already uploaded stay in the folder'
  );
  await page.locator('dialog[data-confirm-action]').getByRole('button', {name: 'Continue'}).click();
  await expect.poll(() => controls.length).toBe(2);
  expect(controls[1]).toEqual({kind: 'gdrive', generation: 1, action: 'disconnect'});
});

test('Administration lists every notebook without a managed backup first and says host backups are not shown', async ({
  page
}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await page.locator('#admin-dialog [data-settings-category=backups]').click();
  const panel = page.locator('#backups-overview-panel');
  await expect(panel).toContainText('Backups an operator runs on the server’s host are not visible to the app');
  await expect(panel).toContainText(/\d+ of \d+ notebooks have no managed backup\./);
  await expect(panel.locator('[data-backup-notebook=alpha]')).toContainText('No managed backup');
  await expect(panel.locator('[data-backup-notebook=default]')).toContainText('No managed backup');

  // A connected destination shows where it goes and when it last verified,
  // after the notebooks without one, in the server's order.
  await page.route('**/api/organization/action/managed_backup_overview', async route => {
    const response = await route.fetch();
    const overview = await response.json();
    const rows = overview.notebooks.map(row =>
      row.project === 'default'
        ? {
            ...row,
            destinations: [
              {
                kind: 'dropbox',
                name: 'Dropbox',
                target: 'ForgetMeNote/default-abc123',
                account: 'owner@example.com',
                enabled: true,
                lastRevision: 'd'.repeat(40),
                lastSuccess: '2026-11-27T10:00:00Z'
              }
            ]
          }
        : row
    );
    rows.sort((a, b) => Number(!b.destinations.length) - Number(!a.destinations.length));
    return route.fulfill({response, json: {...overview, notebooks: rows}});
  });
  await panel.getByRole('button', {name: 'Refresh'}).click();
  await expect(panel.locator('[data-backup-notebook=default]')).toContainText('Dropbox');
  await expect(panel.locator('[data-backup-notebook]').first()).not.toHaveAttribute('data-backup-notebook', 'default');
  await expect(panel.locator('[data-backup-notebook]').last()).toHaveAttribute('data-backup-notebook', 'default');
  await expect(panel.locator('[data-backup-notebook=default]')).toContainText(
    'Dropbox: ForgetMeNote/default-abc123 (owner@example.com) · on · verified'
  );
});
