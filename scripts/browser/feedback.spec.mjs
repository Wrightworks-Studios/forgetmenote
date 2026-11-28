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

test('an error toast has a Dismiss button, waits while hovered or focused and then dismisses itself', async ({
  page
}) => {
  await page.clock.install();
  await app.login(page, 'password');
  await page.route('**/api/note?*', route =>
    route.request().method() === 'GET' && new URL(route.request().url()).searchParams.get('path') === 'Second.md'
      ? route.fulfill({status: 503, json: {error: 'Synthetic note read unavailable'}})
      : route.continue()
  );
  await page.goto(app.origin + '/?project=default&note=Second.md');
  const tray = page.locator('#feedback-tray'),
    message = page.locator('#notice'),
    dismiss = tray.getByRole('button', {name: 'Dismiss'});
  await expect(tray.getByRole('alert')).toHaveText('Synthetic note read unavailable');
  await expect(dismiss).toBeVisible();
  // An error stays long enough to read, and as long as the pointer rests on it.
  await page.clock.fastForward(10000);
  await expect(message).toBeVisible();
  await message.hover();
  await page.clock.fastForward(60000);
  await expect(message).toBeVisible();
  await page.mouse.move(0, 0);
  await page.clock.fastForward(60000);
  await expect(message).toBeHidden();
  await expect(dismiss).toBeHidden();

  await page.locator('#browse-root').click();
  await page.locator('#folder-items a[data-note="Second.md"]').click();
  await expect(tray.getByRole('alert')).toHaveText('Synthetic note read unavailable');
  await dismiss.focus();
  await page.clock.fastForward(60000);
  await expect(message).toBeVisible();
  await page.keyboard.press('Enter');
  await expect(message).toBeHidden();
  await expect(dismiss).toBeHidden();
});

test('the Trash undo toast offers Undo and Dismiss and expires like every toast', async ({page}) => {
  await app.api('/p/default/api/note', {path: 'Toast.md', content: '# Toast\n', revision: ''});
  await page.clock.install();
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&note=Toast.md');
  await expect(page.locator('#preview h1')).toHaveText('Toast');
  const tray = page.locator('#feedback-tray'),
    undo = tray.getByRole('button', {name: 'Undo move to Trash'}),
    dismiss = tray.getByRole('button', {name: 'Dismiss'});
  const trash = async () => {
    await page.locator('#note-details-toggle').click();
    await page.locator('#properties-open').click();
    await page.locator('#note-trash').click();
    await expect(tray.getByRole('status')).toHaveText('Moved to Trash.');
  };
  await trash();
  await expect(undo).toBeVisible();
  await expect(dismiss).toBeVisible();
  // A toast that offers an action outlasts a plain confirmation.
  await page.clock.fastForward(7000);
  await expect(undo).toBeVisible();
  await undo.click();
  await expect(tray.getByRole('status')).toHaveText('Restored Toast.md.');
  await expect(undo).toHaveCount(0);
  await expect(dismiss).toBeVisible();
  // The pointer that clicked Undo rests on the toast until it moves away.
  await page.clock.fastForward(6000);
  await expect(dismiss).toBeVisible();
  await page.mouse.move(0, 0);
  await page.clock.fastForward(6000);
  await expect(page.locator('#notice')).toBeHidden();
  await expect(dismiss).toBeHidden();

  await page.locator('#browse-root').click();
  await page.locator('#folder-items a[data-note="Toast.md"]').click();
  await expect(page.locator('#preview h1')).toHaveText('Toast');
  await trash();
  await page.mouse.move(0, 0);
  await page.clock.fastForward(60000);
  await expect(undo).toBeHidden();
  await expect(dismiss).toBeHidden();
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
