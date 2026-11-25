import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'fmn-feedback-',
    account: {username: 'feedback-admin', password: 'DisposableFeedback123!'}
  });
  await app.api('/p/default/api/note', {path: 'First.md', content: '# First\n', revision: ''});
  await app.api('/p/default/api/note', {path: 'Second.md', content: '# Second\n', revision: ''});
});
test.afterAll(async () => app?.close());
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

test('draft navigation uses an accessible nonblocking confirmation and retains canceled text', async ({page}) => {
  const native = [];
  page.on('dialog', async dialog => {
    native.push(dialog.type());
    await dialog.dismiss();
  });
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&note=First.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  await fillSource(page, '# Private first draft\n');
  await page.locator('#browse-root').click();
  const confirmation = page.getByRole('dialog', {name: 'Confirm action'});
  await expect(confirmation).toBeVisible();
  await expect(confirmation).toContainText('Keep your draft');
  await expect(confirmation.getByRole('button', {name: 'Cancel', exact: true})).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(confirmation).toBeHidden();
  await expectEditorText(page, '# Private first draft\n');
  expect(native).toEqual([]);
});

test('compact writing overlay covers navigation and restores it on close', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&note=First.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await page.locator('#word-count').click();
  await expect(page.locator('#writing-panel')).toBeVisible();
  expect(await page.locator('#mobile-navigation').evaluate(node => node.inert)).toBe(true);
  expect(await page.locator('#main').evaluate(node => node.inert)).toBe(true);
  await page.keyboard.press('Escape');
  await expect(page.locator('#writing-panel')).toBeHidden();
  expect(await page.locator('#mobile-navigation').evaluate(node => node.inert)).toBe(false);
  expect(await page.locator('#main').evaluate(node => node.inert)).toBe(false);
});
