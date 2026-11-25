import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
let app, cookies;
const account = {username: 'composition-admin', password: 'DisposableComposition123!'};
test.beforeAll(async ({browser}) => {
  app = await startApp({name: 'forgetmenote-composition-', account});
  await app.api('/api/projects', {id: 'harvest', name: 'Harvest Valley'});
  for (let i = 0; i < 33; i++)
    await app.api('/p/harvest/api/note', {
      path: i < 28 ? 'wiki/Note ' + i + '.md' : 'Note ' + i + '.md',
      content:
        '---\ntags: [' +
        (i === 28 ? 'world' : 'design') +
        ']\nstatus: Active\n---\n# ' +
        (i === 28 ? 'Harvest Valley' : 'Harvest Valley — ' + i) +
        '\nAn island town shaped by its people, with room for observations and shared ideas.',
      revision: ''
    });
  const context = await browser.newContext();
  try {
    await app.login(await context.newPage(), 'password');
    cookies = await context.cookies();
  } finally {
    await context.close();
  }
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', e => {
    throw e;
  });
  await page.context().addCookies(cookies);
});
test('Reading Room aligns the notebook heading, toolbar and full-width rows with page actions', async ({
  page
}, info) => {
  await page.setViewportSize({width: 1588, height: 990});
  await page.goto(app.origin + '/?project=harvest&folder=');
  await expect(page.locator('#folder-title')).toHaveText('Harvest Valley');
  await expect(page.locator('#search-open')).toBeEnabled();
  await expect(page.locator('#new-note')).toBeEnabled();
  await expect(page.locator('#sidebar [data-folder], #sidebar #new-note')).toHaveCount(0);
  const sidebar = await page.locator('#sidebar').boundingBox(),
    title = await page.locator('#folder-title').boundingBox(),
    left = sidebar.x + sidebar.width + 44;
  for (const selector of ['#folder-title', '#library-toolbar', '#folder-group', '#folder-items', '#folder-crumbs'])
    expect(Math.abs((await page.locator(selector).boundingBox()).x - left)).toBeLessThanOrEqual(4);
  const create = await page.locator('#new-note').boundingBox();
  expect(create.x).toBeGreaterThan(1300);
  expect(Math.abs(create.y - title.y)).toBeLessThan(50);
  const crumb = await page.locator('#folder-crumbs').boundingBox(),
    ask = await page.locator('#chat-open').boundingBox();
  expect(Math.abs(ask.y + ask.height / 2 - crumb.y - crumb.height / 2)).toBeLessThan(8);
  expect(ask.x).toBeLessThan(700);
  await expect(page.locator('#folder-count')).toHaveText('5 notes · 1 folder');
  await expect(page.locator('#count-notes')).toHaveText('33');
  const folder = await page.locator('#folder-groups .browse-item').boundingBox(),
    note = await page.locator('#folder-items .browse-item').first().boundingBox();
  expect(folder.width).toBeCloseTo(note.width, 0);
  expect(folder.width).toBeGreaterThan(1200);
  await expect(page.locator('#folder-items .row-icon')).toHaveCount(5);
  await expect(page.locator('#library-notes-label')).toHaveText('Notes');
  for (const theme of ['light', 'dark']) {
    await page.evaluate(t => (document.documentElement.dataset.theme = t), theme);
    await page.screenshot({path: info.outputPath('reading-room-' + theme + '.png')});
  }
  await page.setViewportSize({width: 2000, height: 990});
  expect((await page.locator('#folder-items').boundingBox()).width).toBeGreaterThan(1600);
});
test('search navigation focuses immediately and library Tags opens working filters', async ({page}) => {
  await page.goto(app.origin + '/?project=harvest&folder=');
  await expect(page.locator('#search-open')).toBeEnabled();
  await expect(page.locator('#search-open')).toHaveAccessibleName('Search notes');
  let release;
  const held = new Promise(resolve => (release = resolve));
  await page.route('**/api/notes?collection=*', async route => {
    await held;
    await route.continue();
  });
  try {
    await page.locator('#search-open').click();
    await expect(page.locator('#search')).toBeFocused();
    await page.keyboard.type('island');
    await expect(page.locator('#search')).toHaveValue('island');
  } finally {
    release();
    await page.unrouteAll({behavior: 'wait'});
  }
  await page.locator('#search-close').click();
  await page.locator('#library-tags').click();
  await expect(page.locator('#tag-filter')).toBeFocused();
  await page.locator('#tag-filter').selectOption('world');
  await expect(page.locator('#search-results [data-note]')).toHaveCount(1);
  await expect(page.locator('#active-search-options')).toContainText('world');
});
test('sidebar footer, page creation and folder navigation remain usable on phones', async ({page}) => {
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 900});
    await page.goto(app.origin + '/?project=harvest&folder=');
    await expect(page.locator('#new-note')).toBeEnabled();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.locator('#create-toggle').click();
    await page.locator('#create-source').click();
    await expect(page.locator('#new-source-panel')).toBeVisible();
    await page.locator('#creation-close').click();
    if (width <= 720) await page.locator('#menu').click();
    for (const id of [
      'inbox-open',
      'browse-root',
      'collection-drafts',
      'collection-archives',
      'review-open',
      'trash-open',
      'settings-open'
    ]) {
      await expect(page.locator('#' + id)).toBeVisible();
      await expect(page.locator('#' + id + ' svg')).toHaveCount(1);
    }
    const account = await page.locator('#account-actions-toggle').boundingBox(),
      bell = await page.locator('#notifications-open').boundingBox();
    expect(Math.abs(account.y - bell.y)).toBeLessThan(8);
    if (width <= 720) await page.locator('#menu').click();
    await page.locator('#folder-groups .browse-item-link').click();
    await expect(page.locator('#folder-title')).toHaveText('wiki');
    await page.locator('#folder-crumbs a').first().click();
    await expect(page.locator('#folder-title')).toHaveText('Harvest Valley');
  }
});
test('sidebar names the instance beside its logo and pins the account row to the bottom', async ({page}) => {
  await page.setViewportSize({width: 1440, height: 900});
  await page.goto(app.origin + '/?project=harvest&folder=');
  const brand = page.locator('#sidebar .brand [data-instance-name]');
  await expect(brand).toHaveText('ForgetMeNote');
  await page.evaluate(() => document.fonts.ready);
  expect(await brand.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
  const name = 'Harvest Valley Cooperative Research Institute';
  await app.api('/api/organization/action/set_instance_branding', {name});
  try {
    for (const [width, height] of [
      [1440, 900],
      [1280, 420],
      [320, 568]
    ]) {
      await page.setViewportSize({width, height});
      await page.goto(app.origin + '/?project=harvest&folder=');
      if (width <= 720) await page.locator('#menu').click();
      const label = page.locator('#sidebar .brand [data-instance-name]');
      await expect(label).toHaveText(name);
      const heading = await page.locator('.sidebar-heading').evaluate(element => {
        const text = element.querySelector('[data-instance-name]'),
          box = text.getBoundingClientRect(),
          controls = [...element.querySelectorAll('button')].filter(button => button.getClientRects().length);
        return {
          left: box.left,
          right: box.right,
          width: box.width,
          logo: element.querySelector('.brand img').getBoundingClientRect().right,
          controls: Math.min(...controls.map(button => button.getBoundingClientRect().left)),
          truncated: text.scrollWidth > text.clientWidth && getComputedStyle(text).textOverflow === 'ellipsis'
        };
      });
      expect(heading.width).toBeGreaterThan(60);
      expect(heading.left).toBeGreaterThanOrEqual(heading.logo);
      expect(heading.right).toBeLessThanOrEqual(heading.controls);
      expect(heading.truncated).toBe(true);
      expect(await page.locator('#sidebar').evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
      const sidebar = await page.locator('#sidebar').boundingBox(),
        account = page.locator('.account-row'),
        pinned = await account.boundingBox();
      expect(pinned.y + pinned.height).toBeLessThanOrEqual(height);
      expect(sidebar.y + sidebar.height - (pinned.y + pinned.height)).toBeLessThan(24);
      await page.locator('#trash-open').scrollIntoViewIfNeeded();
      await expect(page.locator('#trash-open')).toBeInViewport({ratio: 1});
      const trash = await page.locator('#trash-open').boundingBox();
      expect(await account.boundingBox()).toEqual(pinned);
      expect(trash.y + trash.height).toBeLessThanOrEqual(pinned.y);
    }
    const label = page.locator('#sidebar .brand [data-instance-name]');
    for (const theme of ['light', 'dark', 'parchment']) {
      await page.evaluate(value => {
        document.documentElement.dataset.theme = value;
      }, theme);
      const contrast = await label.evaluate(element => {
        const channel = value =>
            value / 255 <= 0.03928 ? value / 255 / 12.92 : ((value / 255 + 0.055) / 1.055) ** 2.4,
          luminance = color => {
            const [red, green, blue] = color.match(/[\d.]+/g).map(Number);
            return 0.2126 * channel(red) + 0.7152 * channel(green) + 0.0722 * channel(blue);
          },
          text = luminance(getComputedStyle(element).color),
          background = luminance(getComputedStyle(document.getElementById('sidebar')).backgroundColor);
        return (Math.max(text, background) + 0.05) / (Math.min(text, background) + 0.05);
      });
      expect(contrast, theme).toBeGreaterThan(7);
    }
  } finally {
    await app.api('/api/organization/action/set_instance_branding', {name: 'ForgetMeNote'});
  }
});
