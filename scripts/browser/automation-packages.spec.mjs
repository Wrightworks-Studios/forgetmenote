import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

test('repository packages require a paused installation and explicit revision update', async ({page}) => {
  const account = {username: 'package-admin', password: 'DisposablePackage123!'};
  const app = await startApp({name: 'fmn-package-ui-', account});
  try {
    const directory = await app.api('/api/admin');
    const admin = directory.users.find(u => u.admin && u.id !== '@local-admin');
    const worker = await app.api('/api/organization/action/create_service_account', {
      name: 'Package worker',
      steward: admin.id
    });
    const revision = 'b'.repeat(40),
      next = 'c'.repeat(40);
    const pkg = {
      project: 'packages',
      source: 'source',
      path: 'automations/example',
      revision: next,
      latest: next,
      digest: 'd'.repeat(64),
      manifest: {
        apiVersion: 'forgetmenote.automation/v1',
        id: 'example',
        name: 'Example package',
        entrypoint: 'main.py',
        runtime: 'python-v1',
        files: ['main.py'],
        capabilities: ['checkpoint', 'notebooks.read'],
        notebooks: {input: 'read'}
      }
    };
    let saved,
      definitionRevision = 'one';
    const status = () => ({
      jobs: saved
        ? [
            {
              id: saved.id,
              kind: 'script',
              project: saved.project,
              spec: saved,
              revision: definitionRevision,
              managedBy: 'instance'
            }
          ]
        : [],
      runs: [],
      pythonConfigured: true
    });
    await page.route('**/api/organization/action/automation_status', r => r.fulfill({json: status()}));
    await page.route('**/api/organization/action/discover_automation_packages', r => {
      expect(r.request().postDataJSON().project).toBe('packages');
      return r.fulfill({json: [pkg]});
    });
    await page.route('**/api/organization/action/install_automation_package', r => {
      const body = r.request().postDataJSON();
      expect(body.spec.disabled).toBe(true);
      expect(body.spec.script.revision).toBe(next);
      expect(body.spec.script.entrypoint).toBe('main.py');
      expect(body.spec.script.image).toBeUndefined();
      saved = body.spec;
      definitionRevision = 'two';
      return r.fulfill({json: {spec: saved, revision: definitionRevision, managedBy: 'instance'}});
    });
    await app.login(page, 'password');
    await page.goto(app.origin + '/?project=default');
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await page.locator('#admin-dialog [data-settings-category=automation]').click();
    const root = page.locator('#integration-jobs');
    await root.getByRole('button', {name: 'Repository packages', exact: true}).click();
    await root.getByLabel('Package notebook', {exact: true}).fill('packages');
    await root.getByRole('button', {name: 'Discover', exact: true}).click();
    await expect(root).toContainText(next);
    await expect(root).toContainText('notebooks.read');
    await page.setViewportSize({width: 390, height: 844});
    const bounds = await page.locator('#admin-dialog').evaluate(e => ({width: e.clientWidth, scroll: e.scrollWidth}));
    expect(bounds.scroll).toBeLessThanOrEqual(bounds.width + 1);
    await root.getByRole('button', {name: 'Prepare paused installation'}).click();
    await expect(root.getByLabel('Enabled', {exact: true})).not.toBeChecked();
    const spec = JSON.parse(await root.getByLabel('Automation JSON').inputValue());
    spec.actor = worker.id;
    spec.project = 'default';
    spec.script.notebooks.input = 'default';
    await root.getByLabel('Automation JSON').fill(JSON.stringify(spec));
    await root.getByRole('button', {name: 'Apply JSON', exact: true}).click();
    // A new installation is sandboxed unless the administrator opts out.
    expect(spec.script).not.toHaveProperty('sandbox');
    await expect(root.getByLabel('Sandbox package code', {exact: false})).toBeChecked();
    await root.getByLabel('Sandbox package code', {exact: false}).uncheck();
    await root.getByRole('button', {name: 'Save automation', exact: true}).click();
    const card = root.locator('[data-automation="example"]');
    await expect(card).toContainText('Paused');
    expect(saved.script.sandbox).toBe(false);
    await expect(card).toContainText('Not sandboxed');
    // A repository refresh is available; the already activated definition remains old.
    saved = {...saved, disabled: false, script: {...saved.script, revision}};
    await root.getByRole('button', {name: 'Refresh runs'}).click();
    await card.getByRole('button', {name: 'Inspect / update package'}).click();
    await root.getByRole('button', {name: 'Discover', exact: true}).click();
    await expect(root).toContainText('Active: ' + revision);
    await expect(root).toContainText('Available: ' + next);
    expect(saved.script.revision).toBe(revision);
    await root.getByRole('button', {name: 'Prepare paused update'}).click();
    const update = JSON.parse(await root.getByLabel('Automation JSON').inputValue());
    expect(update.disabled).toBe(true);
    expect(update.script.revision).toBe(next);
    expect(update.script.notebooks.input).toBe('default');
    // Preparing an update must not silently drop the administrator's opt-out.
    expect(update.script.sandbox).toBe(false);
    await expect(root.getByLabel('Sandbox package code', {exact: false})).not.toBeChecked();
    await page.screenshot({path: test.info().outputPath('package-editor-mobile.png')});
    await page.setViewportSize({width: 1280, height: 900});
    await page.screenshot({path: test.info().outputPath('package-editor-desktop.png')});
  } finally {
    await app.close();
  }
});
