import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

// Routes the app publishes for its views. Back restores the scroll position a
// route had when the app left it, whichever view replaced it: a note, Inbox,
// Search or a settings page.
let app, cookies;
test.beforeAll(async ({browser}) => {
  app = await startApp({name: 'fmn-route-history-', account: {username: 'scroller', password: 'DisposableScroll123!'}});
  for (let i = 0; i < 60; i++)
    await app.api('/p/default/api/note', {
      path: `Long/n${String(i).padStart(2, '0')}.md`,
      content: `# Note ${i}\n`,
      revision: ''
    });
  const body = Array.from({length: 150}, (_, i) => `Paragraph ${i} of a long note.`).join('\n\n');
  await app.api('/p/default/api/note', {
    path: 'LongNote.md',
    content: '# Long note\n\n[Other](Other.md)\n\n' + body + '\n',
    revision: ''
  });
  await app.api('/p/default/api/note', {path: 'Other.md', content: '# Other note\n', revision: ''});
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await app.login(page, 'password');
    cookies = await context.cookies();
  } finally {
    await context.close();
  }
});
test.afterAll(async () => app?.close());
test.beforeEach(async ({context, page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await context.addCookies(cookies);
  await page.setViewportSize({width: 1280, height: 500});
});
const scrollY = page => page.evaluate(() => window.scrollY);
// A DOM click leaves the window where the test scrolled it.
const domClick = locator => locator.evaluate(element => element.click());
async function scrolledFolder(page) {
  await page.goto(app.origin + '/?project=default&folder=Long');
  await expect(page.locator('#folder-items a[data-note="Long/n59.md"]')).toBeAttached();
  await page.evaluate(() => window.scrollTo(0, 400));
  await expect.poll(() => scrollY(page)).toBe(400);
}
async function backToFolder(page) {
  await page.goBack();
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect.poll(() => scrollY(page)).toBe(400);
}

test('Back from Inbox restores the folder scroll it left', async ({page}) => {
  await scrolledFolder(page);
  await domClick(page.locator('#inbox-open'));
  await expect(page.locator('#inbox-view')).toBeVisible();
  await backToFolder(page);
});

test('opening Inbox keeps the folder scroll recorded when Back returned to it', async ({page}) => {
  await scrolledFolder(page);
  await domClick(page.locator('#folder-items a[data-note="Long/n30.md"]'));
  await expect(page.locator('#path')).toHaveText('Note 30');
  await backToFolder(page);
  await domClick(page.locator('#inbox-open'));
  await expect(page.locator('#inbox-view')).toBeVisible();
  await backToFolder(page);
});

test('Back from Search restores the folder scroll it left', async ({page}) => {
  await scrolledFolder(page);
  await domClick(page.locator('#search-open'));
  await expect(page.locator('#search-view')).toBeVisible();
  await backToFolder(page);
});

test('Back from a settings page restores the note scroll it left', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=LongNote.md');
  await expect(page.locator('#path')).toHaveText('Long note');
  await page.evaluate(() => window.scrollTo(0, 600));
  await expect.poll(() => scrollY(page)).toBe(600);
  await domClick(page.locator('#settings-open'));
  await expect(page).toHaveURL(/page=notebook/);
  await page.goBack();
  await expect(page.locator('#settings-dialog')).toBeHidden();
  await expect(page.locator('#path')).toHaveText('Long note');
  await expect.poll(() => scrollY(page)).toBe(600);
});

test('Inbox publishes its own address, replacing a stale fragment without another entry', async ({page}) => {
  await page.goto(app.origin + '/?area=inbox&project=default#stale');
  await expect(page.locator('#inbox-view')).toBeVisible();
  const address = () => page.evaluate(() => location.search + location.hash);
  await expect.poll(address).toBe('?area=inbox&project=default');
  const entries = await page.evaluate(() => history.length);
  await domClick(page.locator('#inbox-open'));
  await expect(page.locator('#inbox-view')).toBeVisible();
  expect(await address()).toBe('?area=inbox&project=default');
  expect(await page.evaluate(() => history.length)).toBe(entries);
});
