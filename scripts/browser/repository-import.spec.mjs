import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
const account = {username: 'repository-admin', password: 'DisposableRepositories123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'fmn-repository-import-', account});
});
test.afterAll(async () => app?.close());
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

test('an Admin imports a repository the Sources App is installed on, through import_repository', async ({
  page
}, testInfo) => {
  await app.login(page, 'password');
  await page.goto(app.origin);
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await page.locator('#admin-dialog [data-settings-category=repositories]').click();
  const panel = page.locator('#admin-dialog [data-settings-panel=repositories]');
  await expect(panel.getByRole('heading', {name: 'Repository notebooks', exact: true})).toBeVisible();
  await expect(page.locator('#repository-import-app')).toHaveText(
    'No Sources App is configured on this instance, so imports are refused.'
  );
  await panel.getByLabel('Notebook name', {exact: true}).fill('Example source');
  await expect(panel.getByLabel('Notebook ID', {exact: true})).toHaveValue('example-source');
  await panel.getByLabel('Installation ID', {exact: true}).fill('7');
  await panel.getByLabel('Repository ID', {exact: true}).fill('42');
  const share = panel.getByLabel("Share source files with this notebook's members", {exact: true});
  // Without sharing consent the form is not sent.
  const requests = [];
  page.on('request', request => {
    if (request.url().endsWith('/integrations/actions/import_repository')) requests.push(request.postDataJSON());
  });
  await panel.getByRole('button', {name: 'Import repository', exact: true}).click();
  expect(await share.evaluate(input => input.validity.valueMissing)).toBe(true);
  expect(requests).toEqual([]);
  await share.check();
  // The server's own refusal: this instance has no App to check access with.
  await panel.getByRole('button', {name: 'Import repository', exact: true}).click();
  await expect(page.locator('#repository-import-status')).toHaveText('GitHub integration unavailable');
  expect(requests).toEqual([
    {
      id: 'example-source',
      name: 'Example source',
      installation: 7,
      repository: 42,
      shareWithNotebook: true,
      externalProcessing: false
    }
  ]);

  // A GitHub that accepts the import answers with the source's state; GitHub
  // itself is out of reach here, so the answer is synthetic.
  await page.route('**/integrations/actions/import_repository', route =>
    route.fulfill({
      json: {
        id: 'synthetic-source',
        project: 'example-source',
        connected: true,
        repository: {id: 42, full_name: 'example/source', default_branch: 'main'}
      }
    })
  );
  await panel.getByLabel('Allow external processing of source files', {exact: true}).check();
  await panel.getByRole('button', {name: 'Import repository', exact: true}).click();
  await expect(page.locator('#repository-import-status')).toHaveText(
    'Imported example/source into Example source. Files appear as the first download finishes.'
  );
  expect(requests.at(-1)).toEqual({
    id: 'example-source',
    name: 'Example source',
    installation: 7,
    repository: 42,
    shareWithNotebook: true,
    externalProcessing: true
  });
  await expect(panel.getByLabel('Notebook name', {exact: true})).toHaveValue('');
  await page.setViewportSize({width: 390, height: 844});
  expect(await panel.evaluate(e => e.scrollWidth <= e.clientWidth + 1)).toBe(true);
  await page.screenshot({path: testInfo.outputPath('repositories-mobile.png'), fullPage: true});
});
