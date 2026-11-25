import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({name: 'fmn-assets-', account: {username: 'assets-admin', password: 'DisposableAssets123!'}});
  await app.api('/p/default/api/note', {
    path: 'Welcome.md',
    content: '# Welcome\n\nAn ordinary notebook note.\n',
    revision: ''
  });
});
test.afterAll(async () => app?.close());

test('fingerprinted app preserves lazy editor and isolated writing worker', async ({page}) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await app.login(page, 'password');
  const requests = [];
  page.on('request', request => {
    if (request.url().includes('/static/')) requests.push(request.url());
  });
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  expect(
    requests.filter(url => /\/(source-editor|rendered-preview|source-view|writing-worker)-.*\.js$/.test(url))
  ).toEqual([]);
  const script = await page.locator('script[type=module]').getAttribute('src');
  expect(script).toMatch(/^\/static\/dist\/app-[A-Z0-9]+\.js$/);
  const response = await page.request.get(app.origin + script);
  expect(response.headers()['cache-control']).toBe('public, max-age=31536000, immutable');
  await page.goto(app.origin + '/?project=default&note=Welcome.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  // Visual is the live preview, in the note editor's own chunk.
  await expect(page.locator('#editor')).toHaveAttribute('aria-label', 'Note content');
  await page.locator('#word-count').click();
  await expect(page.locator('#writing-stats')).toContainText('Words');
  expect(requests.some(url => /\/source-editor-.*\.js$/.test(url))).toBe(true);
  // Worker network requests are owned by their worker, so prove its observable
  // result rather than relying on a page-only request observer.
  await expect(page.locator('#word-count')).toContainText('words');
  expect(errors).toEqual([]);
  expect(requests.filter(url => /\.(ttf|woff2)$/.test(url)).every(url => url.endsWith('.woff2'))).toBe(true);
});

test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

test('legacy entry URLs resolve their bundled dependencies in the browser', async ({page}) => {
  await app.login(page, 'password');
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route(app.origin + '/?project=default&note=Welcome.md', async route => {
    const response = await route.fetch();
    const html = (await response.text())
      .replace(/\/static\/dist\/app-[A-Z0-9]+\.js/g, '/static/app.mjs')
      .replace(/\/static\/dist\/style-[A-Z0-9]+\.css/g, '/static/style.css');
    await route.fulfill({response, body: html});
  });
  await page.goto(app.origin + '/?project=default&note=Welcome.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expect(page.locator('#editor')).toHaveAttribute('aria-label', 'Note content');
  expect(errors).toEqual([]);
});

test('startup loads each library navigation dataset once', async ({page}) => {
  await app.login(page, 'password');
  const requests = [];
  page.on('request', request => {
    requests.push(new URL(request.url()));
  });
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect(page.locator('#new-note')).toBeEnabled();
  for (const [name, match] of [
    ['note navigation', url => url.pathname.endsWith('/api/notes') && !url.search],
    ['collection notes', url => url.pathname.endsWith('/api/notes') && url.searchParams.get('collection') === 'notes'],
    ['collection counts', url => url.pathname.endsWith('/api/collections')]
  ])
    expect(requests.filter(match), name + ' duplicate startup request').toHaveLength(1);
});

test('the supported raw chat entry embeds its full module dependency closure', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  const result = await page.evaluate(async () => {
    try {
      const module = await import('/static/chat.mjs');
      return typeof module.initChat;
    } catch (error) {
      return error.message;
    }
  });
  expect(result).toBe('function');
});

test('slow web fonts do not change visible text after first paint', async ({page}) => {
  await app.login(page, 'password');
  let releaseFonts;
  const fontGate = new Promise(resolve => {
    releaseFonts = resolve;
  });
  await page.route('**/*.woff2', async route => {
    await fontGate;
    await route.continue();
  });
  try {
    await page.goto(app.origin + '/?project=default&folder=', {waitUntil: 'domcontentloaded'});
    await expect(page.locator('#new-note')).toBeEnabled();
    const before = await page.evaluate(async () => {
      const sample = document.createElement('span');
      sample.id = 'font-load-sample';
      sample.textContent = 'A dependable notebook for everyone 0123456789';
      sample.style.cssText = 'display:inline-block;white-space:nowrap';
      document.body.append(sample);
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      return sample.getBoundingClientRect().width;
    });
    // Let the initial font blocking period expire while the response is held.
    await page.waitForTimeout(250);
    releaseFonts();
    await page.evaluate(() => document.fonts.ready);
    const after = await page.locator('#font-load-sample').evaluate(element => element.getBoundingClientRect().width);
    expect(after).toBe(before);
  } finally {
    releaseFonts();
  }
});

test('pages preload the main web fonts once, as the stylesheet uses them', async ({page}) => {
  const fonts = [];
  page.on('request', request => {
    if (request.url().endsWith('.woff2')) fonts.push(new URL(request.url()).pathname);
  });
  await page.goto(app.origin + '/');
  const preloaded = await page
    .locator('link[rel=preload][as=font]')
    .evaluateAll(links => links.map(link => ({path: new URL(link.href).pathname, crossOrigin: link.crossOrigin})));
  expect(preloaded.map(link => link.path)).toEqual([
    expect.stringMatching(/^\/static\/dist\/assets\/Onest-[A-Z0-9]{8}\.woff2$/),
    expect.stringMatching(/^\/static\/dist\/assets\/Fraunces-[A-Z0-9]{8}\.woff2$/)
  ]);
  for (const link of preloaded) expect(link.crossOrigin).toBe('anonymous');
  await page.evaluate(() => document.fonts.ready);
  // A preload that does not match the stylesheet's request is fetched twice.
  for (const link of preloaded) expect(fonts.filter(path => path === link.path)).toHaveLength(1);
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('link[rel=preload][as=font]')).toHaveCount(3);
  expect(await page.evaluate(() => document.fonts.check('16px Onest'))).toBe(true);
});
