import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({name: 'fmn-people-', account: {username: 'people-admin', password: 'DisposablePeople123!'}});
});
test.afterAll(async () => app?.close());
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

test('comments and history show people and human timestamps without synthetic emails', async ({page}) => {
  await app.login(page, 'password');
  const me = await (await page.request.get(app.origin + '/api/me')).json();
  await app.api('/api/users', {id: me.id, displayName: 'Alex Writer'});
  const noteResponse = await page.request.post(app.origin + '/p/default/api/note', {
    headers: {Origin: app.origin},
    data: {path: 'People.md', content: '# People\n', revision: ''}
  });
  expect(noteResponse.ok()).toBe(true);
  const note = await noteResponse.json();
  const commentResponse = await page.request.post(app.origin + '/p/default/api/comments', {
    headers: {Origin: app.origin},
    data: {path: note.path, text: 'A readable person', revision: note.revision}
  });
  expect(commentResponse.ok()).toBe(true);
  await page.goto(app.origin + '/?project=default&note=People.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#note-details-toggle').click();
  await page.locator('#comments-open').click();
  await expect(page.locator('.comment-meta')).toContainText('Alex Writer');
  const time = page.locator('.comment-meta time');
  await expect(time).toHaveAttribute('datetime', /T/);
  await expect(time).toHaveAttribute('title', /.+/);
  await expect(page.locator('.comment-meta')).not.toContainText(me.id);
  await page.locator('#comments-list').getByRole('button', {name: 'Reply', exact: true}).click();
  await expect(page.locator('#comment-label')).toHaveText('Reply to Alex Writer');
  await page.locator('#comment-reset').click();
  await page.keyboard.press('Escape');
  await page.locator('#note-details-toggle').click();
  await page.locator('#history-open').click();
  await expect(page.locator('.history-attribution').first()).toContainText('Alex Writer');
  await expect(page.locator('#history-list')).not.toContainText('@users.gitnotes.invalid');
  await expect(page.locator('#history-list')).not.toContainText('User ');
  await page.locator('#history-dialog [data-close=history-dialog]').click();
  await page.locator('#account-actions-toggle').click();
  await page.locator('#account-open').click();
  await expect(page.locator('#account-author')).toContainText('Alex Writer');
  await expect(page.locator('#account-user')).not.toContainText(me.id);
  await expect(page.locator('#account-dialog')).not.toContainText('@users.gitnotes.invalid');
});
