import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
const account = {username: 'search-ranking-admin', password: 'DisposableSearchRank123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'forgetmenote-search-ranking-', account});
  for (const [path, content] of [
    ['Garden.md', '# Garden\n\nThe drip irrigation runs on a weekly schedule for the tomatoes.\n'],
    ['Produce.md', '# Produce\n\nTomatoes ripen in August.\n']
  ])
    await app.api('/p/default/api/note', {path, content, revision: ''});
});
test.afterAll(async () => {
  await app?.close();
});

test('automatic search ranks keywords locally and Enter asks Jev once', async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await app.login(page, 'password');
  const bodies = [];
  await page.route('**/p/default/api/knowledge/hybrid-search', async route => {
    const body = route.request().postDataJSON();
    bodies.push(body);
    const response = await route.fetch();
    const result = await response.json();
    // The synthetic instance has no Jev service; the server-side contract is
    // covered by Go tests. This checks what the page sends and shows.
    result.rerankAvailable = true;
    if (body.rerank) {
      result.matches.reverse();
      result.reranked = true;
    }
    await route.fulfill({response, json: result});
  });
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.locator('#search-open').click();
  await page.locator('#search').fill('tomatoes irrigation schedule');
  const results = page.locator('#search-results .note-row a');
  await expect(results).toHaveCount(2);
  await expect(results.first()).toContainText('Garden');
  await expect(page.locator('#search-status')).toContainText('Press Enter to rank with Jev');
  expect(bodies.length).toBeGreaterThan(0);
  expect(bodies.every(body => !body.rerank)).toBe(true);

  const ranked = page.waitForRequest(
    request => request.url().endsWith('/api/knowledge/hybrid-search') && request.postDataJSON().rerank === true
  );
  await page.locator('#search').press('Enter');
  await ranked;
  await expect(page.locator('#search-status')).toContainText('Ranked by Jev');
  await expect(results.first()).toContainText('Produce');
  expect(bodies.filter(body => body.rerank).length).toBe(1);

  await page.getByRole('button', {name: 'Search filters'}).click();
  await page.locator('#advanced-search summary').click();
  await page.getByRole('radio', {name: 'Text'}).check();
  await expect(page.locator('#search-status')).toContainText('text match');
  const before = bodies.length;
  await page.locator('#search').press('Enter');
  await expect.poll(() => bodies.length).toBeGreaterThan(before);
  expect(bodies.slice(before).every(body => !body.rerank)).toBe(true);
  await expect(page.locator('#search-status')).not.toContainText('Jev');
});
