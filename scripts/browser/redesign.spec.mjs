import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';
let app, cookies;
const account = {username: 'reading-room-admin', password: 'DisposableReading123!'};
test.beforeAll(async ({browser}) => {
  app = await startApp({name: 'forgetmenote-redesign-', account});
  for (const [path, content] of [
    ['Garden/index.md', '# Garden\nAn overview of the garden.'],
    [
      'Garden/field-notes.md',
      '---\ntitle: A small garden of ideas\ntags: [garden, observation]\n---\n# A small garden of ideas\nA place to collect observations and let ideas grow.\n\n## Morning light\nThe flowers opened after the rain.\n\n## References\n[Garden overview](index.md)\n\n## Next season\nKeep a little room for something unexpected.'
    ],
    ['Welcome.md', '# Welcome to your reading room\nFind a quiet moment to read, write, and connect your ideas.']
  ])
    await app.api('/p/default/api/note', {path, content, revision: ''});
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await app.login(page, 'password');
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
test('library defaults to rows, separates folders, honors cards and restores the originating folder', async ({
  page
}, info) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#folder-items')).toBeVisible();
  await page.screenshot({path: info.outputPath('library.png'), fullPage: true});
  await expect(page.locator('#folder-items')).toHaveAttribute('data-view', 'list');
  await expect(page.locator('#folder-groups [data-kind=folder]')).toHaveCount(1);
  await expect(page.locator('#sidebar a[data-note]')).toHaveCount(0);
  await expect(page.locator('#folder-browser').getByRole('button', {name: 'New note', exact: true})).toBeVisible();
  await page.getByRole('button', {name: 'Cards', exact: true}).click();
  await page.reload();
  await expect(page.locator('#folder-items')).toHaveAttribute('data-view', 'cards');
  await page
    .locator('#folder-groups')
    .getByRole('link', {name: /Garden/})
    .click();
  await page
    .locator('#folder-items')
    .getByRole('link', {name: /A small garden/})
    .click();
  await page.locator('#note-back').click();
  await expect(page.locator('#folder-title')).toHaveText('Garden');
});
test('context shares one surface, follows headings and preserves the editor', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
  await page.route('**/api/live*', r => r.abort());
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('#note-context-contents')).toContainText('Morning light');
  await page.locator('#note-context-contents').getByRole('link', {name: 'Morning light', exact: true}).click();
  await expect(page).toHaveURL(/#morning-light$/);
  await page.locator('[data-context-tab=references]').click();
  await expect(page.locator('#note-context-references')).toContainText('Garden overview');
  await page.keyboard.press('Escape');
  await expect(page.locator('#comments-panel')).toBeHidden();
  await expect(page.locator('#note-context-toggle')).toBeFocused();
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  await fillSource(page, 'An exact private draft.');
  await page.locator('#search-open').click();
  await page.locator('#search').fill('flowers');
  await expect(page.locator('#search-results')).toContainText('garden');
  await page.locator('#search-close').click();
  await expectEditorText(page, 'An exact private draft.');
});
test('phone destinations and both themes fit 320 through 1440 pixels', async ({page}, info) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#search-open')).toBeEnabled();
  for (const theme of ['light', 'dark'])
    for (const width of [320, 390, 768, 1024, 1440]) {
      await page.setViewportSize({width, height: 900});
      await page.evaluate(t => (document.documentElement.dataset.theme = t), theme);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      if (width <= 720) {
        await expect(page.locator('#mobile-navigation')).toBeVisible();
        await expect(page.locator('#mobile-navigation button')).toHaveCount(4);
      }
      await page.screenshot({path: info.outputPath(`${theme}-${width}.png`), fullPage: true});
    }
});
test('Back and browser history restore search filters and folder scroll', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=Garden');
  await page.locator('#search-open').click();
  await page.locator('#search').fill('flowers');
  await expect(page.locator('#search-results a[data-note]')).toHaveCount(1);
  await page.locator('#search-options-toggle').click();
  await page.locator('#tag-filter').selectOption('garden');
  await expect(page.getByRole('button', {name: 'Remove Tag: garden'})).toBeVisible();
  await page.locator('#search-results a[data-note]').click();
  await expect(page.locator('#document')).toBeVisible();
  await page.locator('#note-back').click();
  await expect(page.locator('#search')).toHaveValue('flowers');
  await expect(page.locator('#tag-filter')).toHaveValue('garden');
  await page.locator('#search-close').click();
  await page.goBack();
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page.locator('#search')).toHaveValue('flowers');
  await expect(page.locator('#tag-filter')).toHaveValue('garden');
});
test('closed and superseded reference requests cannot populate a newer context', async ({page}) => {
  let release, entered;
  const held = new Promise(resolve => (entered = resolve)),
    gate = new Promise(resolve => (release = resolve));
  await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
  await page.route('**/api/knowledge/evidence?**', async route => {
    entered();
    await gate;
    await route.fulfill({json: {sources: [], backlinks: [], limitations: 'RETIRED REFERENCE'}});
  });
  try {
    await page.locator('#note-details-toggle').click();
    await page.locator('#sources-open').click();
    await held;
    await page.locator('[data-context-tab=comments]').click();
    release();
    await expect(page.locator('#note-context-comments')).toBeVisible();
    await expect(page.locator('#note-context-references')).not.toContainText('RETIRED REFERENCE');
    await page.locator('#comments-close').click();
    await expect(page.locator('#note-details-toggle')).toBeFocused();
  } finally {
    release();
  }
});
test('reader, sheets, keyboard focus and saved themes fit the available viewport', async ({page}, info) => {
  await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
  await expect(page.locator('#new-note')).toBeEnabled();
  for (const theme of ['light', 'dark', 'parchment'])
    for (const width of [320, 390, 768, 1024, 1440]) {
      await page.setViewportSize({width, height: 900});
      await page.evaluate(t => (document.documentElement.dataset.theme = t), theme);
      await page.locator('#note-context-toggle').click();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      await page.screenshot({path: info.outputPath(`reader-${theme}-${width}.png`), fullPage: true});
      await page.keyboard.press('Escape');
      await expect(page.locator('#comments-panel')).toBeHidden();
      await expect(page.locator('#note-context-toggle')).toBeFocused();
    }
  await page.setViewportSize({width: 390, height: 500});
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  await expect(page.locator('#mobile-navigation')).toBeHidden();
  await expect(page.locator('#save')).toBeVisible();
});
test('long search and folder results return to their scroll position', async ({page}) => {
  for (let i = 0; i < 22; i++)
    await app.api('/p/default/api/note', {
      path: 'Scrolling/Note ' + String(i).padStart(2, '0') + '.md',
      content: '# Observation ' + i + '\nscrollcontinuityfixture A long enough excerpt to scan in the library.',
      revision: ''
    });
  await page.goto(app.origin + '/?project=default&folder=Scrolling');
  await expect(page.locator('#folder-items [data-note]')).toHaveCount(22);
  const last = page.locator('#folder-items [data-note]').last();
  await last.scrollIntoViewIfNeeded();
  const folderScroll = await page.evaluate(() => scrollY);
  await last.click();
  await expect(page.locator('#document')).toBeVisible();
  await page.locator('#note-back').click();
  await expect.poll(() => page.evaluate(() => scrollY)).toBe(folderScroll);
  await page.locator('#search-open').click();
  await page.locator('#search').fill('scrollcontinuityfixture');
  await expect(page.locator('#search-results [data-note]')).toHaveCount(22);
  const result = page.locator('#search-results [data-note]').last();
  await result.scrollIntoViewIfNeeded();
  const searchScroll = await page.evaluate(() => scrollY);
  await result.click();
  await expect(page.locator('#document')).toBeVisible();
  await page.locator('#note-back').click();
  await expect.poll(() => page.evaluate(() => scrollY)).toBe(searchScroll);
});
test('editor formatting stays below Save while scrolling a long document', async ({page}) => {
  const path = 'Long editor.md';
  await app.api('/p/default/api/note', {
    path,
    content:
      '# Long editor\n\n' +
      Array.from({length: 40}, (_, i) => 'Paragraph ' + i + ' stays available while writing.').join('\n\n'),
    revision: ''
  });
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await page.locator('#mode').click();
  await page.locator('#format-visual').click();
  for (const width of [390, 1440]) {
    await page.setViewportSize({width, height: 500});
    await page.evaluate(() => scrollTo(0, 600));
    await expect
      .poll(async () => {
        const header = await page.locator('#main > header').boundingBox(),
          toolbar = await page.locator('#editor-tools').boundingBox();
        return toolbar.y >= header.y + header.height - 1 && toolbar.y + toolbar.height <= 500;
      })
      .toBe(true);
    await expect(page.locator('#save')).toBeVisible();
    await page.locator('#format-source').click();
    await page.locator('#format-visual').click();
  }
});
test('a phone tag opens unobstructed search and returns to the same note', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
  await expect(page.locator('#sidebar')).toBeHidden();
  await page.locator('#metadata-bar').getByRole('button', {name: '#garden', exact: true}).click();
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page.locator('#sidebar')).toBeHidden();
  await expect(page.locator('#tag-filter')).toHaveValue('garden');
  await page.locator('#search-close').click();
  await expect(page.locator('#note-path')).toHaveText('Garden/field-notes.md');
  await expect(page.locator('#document')).toBeVisible();
});
test('long folder names and document titles fit compact library and reader chrome', async ({page}) => {
  const folder = 'LongFolderName'.repeat(10),
    title = 'LongDocumentTitle'.repeat(14),
    path = folder + '/nested/note.md';
  await app.api('/p/default/api/note', {
    path,
    content: '---\ntitle: ' + title + '\n---\n# ' + title + '\n\nA readable paragraph.',
    revision: ''
  });
  await page.setViewportSize({width: 320, height: 700});
  await page.goto(app.origin + '/?project=default&folder=' + encodeURIComponent(folder));
  await expect(page.locator('#folder-title')).toHaveText(folder);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#path')).toHaveText(title);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  expect((await page.locator('#main > header').boundingBox()).height).toBeLessThan(210);
});
test('typing invalidates the previous search results before the next request starts', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=Garden');
  await page.locator('#search-open').click();
  await expect(page.locator('#search-results [data-note]')).not.toHaveCount(0);
  const count = await page.locator('#search').evaluate(input => {
    input.value = 'a query with no previous results';
    input.dispatchEvent(new Event('input', {bubbles: true}));
    return document.querySelectorAll('#search-results [data-note]').length;
  });
  expect(count).toBe(0);
});

test('browser Back restores a long folder through repeated Forward navigation', async ({page}) => {
  for (let i = 0; i < 24; i++)
    await app.api('/p/default/api/note', {
      path: 'History scroll/Note ' + String(i).padStart(2, '0') + '.md',
      content: '# History note ' + i + '\n\n' + 'A synthetic paragraph for browser history.\n\n'.repeat(60),
      revision: ''
    });
  await page.setViewportSize({width: 1440, height: 900});
  await page.goto(app.origin + '/?project=default&folder=History%20scroll');
  await expect(page.locator('#folder-items [data-note]')).toHaveCount(24);
  const last = page.locator('#folder-items [data-note]').last();
  await last.scrollIntoViewIfNeeded();
  const scroll = await page.evaluate(() => scrollY);
  expect(scroll).toBeGreaterThan(1000);
  await last.click();
  await expect(page.locator('#document')).toBeVisible();
  for (let i = 0; i < 2; i++) {
    await page.goBack();
    await expect(page.locator('#folder-items [data-note]')).toHaveCount(24);
    await expect
      .poll(() => page.evaluate(() => scrollY), {message: 'Folder scroll after Back ' + (i + 1)})
      .toBe(scroll);
    if (i === 0) {
      await page.goForward();
      await expect(page.locator('#document')).toBeVisible();
    }
  }
});
for (const [from, to] of [
  [390, 768],
  [1024, 390]
])
  test('context keeps the background inaccessible across ' + from + ' to ' + to + ' pixels', async ({page}) => {
    await page.setViewportSize({width: from, height: 844});
    await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
    await page.locator('#note-details-toggle').click();
    await page.locator('#sources-open').click();
    await page.locator('[data-context-tab=comments]').click();
    await page.locator('#comment-text').fill('Unsent comment across a resize.');
    await page.setViewportSize({width: to, height: 844});
    await expect(page.locator('#comments-panel')).toHaveAttribute('aria-modal', 'true');
    for (const id of ['main', 'sidebar', 'mobile-navigation'])
      await expect(page.locator('#' + id)).toHaveJSProperty('inert', true);
    await page.locator('#mode').evaluate(node => node.focus());
    await expect(page.locator('#mode')).not.toBeFocused();
    await expect(page.locator('#note-back').click({timeout: 300})).rejects.toThrow();
    await expect(page.locator('#document')).toBeVisible();
    await page.locator('#comments-close').focus();
    await page.keyboard.press('Tab');
    await expect(page.locator('[data-context-tab=contents]')).toBeFocused();
    await page.setViewportSize({width: 1440, height: 844});
    await expect(page.locator('#comments-panel')).not.toHaveAttribute('aria-modal', 'true');
    await expect(page.locator('#main')).toHaveJSProperty('inert', false);
    await expect(page.locator('#sidebar')).toHaveJSProperty('inert', false);
    await page.setViewportSize({width: 390, height: 844});
    await expect(page.locator('#main')).toHaveJSProperty('inert', true);
    await page.keyboard.press('Escape');
    await expect(page.locator('#comments-panel')).toBeHidden();
    await expect(page.locator('#note-details-toggle')).toBeFocused();
    for (const id of ['main', 'sidebar', 'mobile-navigation'])
      await expect(page.locator('#' + id)).toHaveJSProperty('inert', false);
    await page.locator('#note-details-toggle').click();
    await page.locator('#comments-open').click();
    await expect(page.locator('#comment-text')).toHaveValue('Unsent comment across a resize.');
    await page.keyboard.press('Escape');
    await page.locator('#menu').click();
    await expect(page.locator('#main')).toHaveJSProperty('inert', true);
    await page.keyboard.press('Escape');
    await expect(page.locator('#main')).toHaveJSProperty('inert', false);
  });

test('chat keeps shared background ownership while resizing and using a native dialog', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
  await page.locator('#chat-open').click();
  await page.setViewportSize({width: 768, height: 844});
  await expect(page.locator('#chat-panel')).toHaveAttribute('aria-modal', 'true');
  await expect(page.locator('#main')).toHaveJSProperty('inert', true);
  await page.locator('#mode').evaluate(node => node.focus());
  await expect(page.locator('#mode')).not.toBeFocused();
  await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
  await expect(page.locator('#chat-settings')).toBeVisible();
  await page.setViewportSize({width: 390, height: 844});
  await page.keyboard.press('Escape');
  await expect(page.locator('#chat-settings')).toBeHidden();
  await expect(page.locator('#chat-panel')).toBeVisible();
  await expect(page.locator('#main')).toHaveJSProperty('inert', true);
  await page.keyboard.press('Escape');
  await expect(page.locator('#chat-panel')).toBeHidden();
  await expect(page.locator('#main')).toHaveJSProperty('inert', false);
  await page.setViewportSize({width: 1440, height: 844});
  await page.locator('#chat-open').click();
  await page.locator('#note-details-toggle').click();
  await page.locator('#sources-open').click();
  await expect(page.locator('#chat-panel')).toBeHidden();
  await expect(page.locator('#comments-panel')).toBeVisible();
  await page.locator('#chat-open').click();
  await expect(page.locator('#comments-panel')).toBeHidden();
  await expect(page.locator('#chat-panel')).toBeVisible();
  await expect(page.locator('#main')).toHaveJSProperty('inert', false);
});
test('phone library exposes creation, compact folders and unambiguous search chrome', async ({page}) => {
  await app.api('/api/projects', {id: 'compact-library', name: 'Compact library'});
  for (const folder of ['Garden', 'Books', 'Ideas', 'Projects', 'Travel'])
    await app.api('/p/compact-library/api/note', {
      path: folder + '/example.md',
      content: '# Example\nA synthetic note.',
      revision: ''
    });
  await app.api('/p/compact-library/api/note', {
    path: 'Root.md',
    content: '# A root note\nVisible below five folders.',
    revision: ''
  });
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(app.origin + '/?project=compact-library&folder=');
  await expect(page.getByRole('button', {name: 'New note', exact: true})).toBeEnabled();
  await page.getByRole('button', {name: 'New note', exact: true}).click();
  await expect(page.locator('#new-dialog')).toBeVisible();
  await page.locator('#creation-close').click();
  await expect
    .poll(async () => {
      const note = await page.locator('#folder-items [data-note]').first().boundingBox(),
        navigation = await page.locator('#mobile-navigation').boundingBox();
      return note && navigation && note.y + note.height <= navigation.y;
    })
    .toBe(true);
  await expect(page.locator('#folder-count')).toContainText(/notes? · /);
  await page.locator('#menu').click();
  await expect(page.locator('#sidebar [data-folder]')).toHaveCount(0);
  await page.locator('#menu').click();
  await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
  // A note that finishes opening leaves any view opened over it, search included.
  await expect(page.getByRole('heading', {name: 'A small garden of ideas', level: 1})).toBeVisible();
  await page.keyboard.press('Control+k');
  await expect(page.locator('#main > header .breadcrumb')).toBeHidden();
  await expect(page.locator('#search-results-title')).toHaveText(/\/Search$/);
  await expect(page.locator('#note-back')).toBeHidden();
  await expect(page.locator('#search-close')).toBeVisible();
  await expect(page.locator('#search-results-status')).toHaveCount(0);
});
test('phone reader places actions below the document and clears the header for heading navigation', async ({page}) => {
  const content =
    '# Anchor test\n\n' +
    'A paragraph to make room for heading navigation.\n\n'.repeat(25) +
    '## Design principles\n\n' +
    'More reading after the heading.\n\n'.repeat(25);
  await app.api('/p/default/api/note', {path: 'Anchors.md', content, revision: ''});
  for (const width of [320, 390]) {
    await page.setViewportSize({width, height: 844});
    await page.goto(app.origin + '/?project=default&note=Anchors.md');
    await page.evaluate(() => document.documentElement.style.setProperty('--reading-size', '28px'));
    await expect(page.locator('#mobile-navigation')).toBeHidden();
    for (const id of ['mode', 'chat-open', 'note-details-toggle'])
      expect((await page.locator('#' + id).boundingBox()).y).toBeGreaterThan(740);
    expect((await page.locator('#main > header').boundingBox()).height).toBeLessThan(90);
    await page.locator('#note-context-toggle').click();
    await page.locator('#note-context-contents').getByRole('link', {name: 'Design principles'}).click();
    const header = await page.locator('#main > header').boundingBox(),
      heading = await page.locator('#h-design-principles').boundingBox();
    expect(heading.y).toBeGreaterThanOrEqual(header.y + header.height + 8);
    await page.locator('#note-context-toggle').click();
    await expect(page.locator('#note-context-contents [aria-current="location"]')).toHaveText('Design principles');
    await page.keyboard.press('Escape');
    await page.goto(app.origin + '/?project=default&note=Anchors.md#design-principles');
    await expect
      .poll(async () => {
        const h = await page.locator('#h-design-principles').boundingBox(),
          bar = await page.locator('#main > header').boundingBox();
        return h && h.y >= bar.height + 8 && h.y < 500;
      })
      .toBe(true);
  }
});

test('phone formatting exposes common tools and expands the remaining controls without losing text', async ({page}) => {
  await page.setViewportSize({width: 320, height: 600});
  await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
  await page.route('**/api/live*', r => r.abort());
  await page.locator('#mode').click();
  await page.locator('#format-visual').click();
  const bold = page.getByRole('button', {name: 'Bold', exact: true});
  await expect(bold).toBeInViewport();
  await expect(page.getByRole('button', {name: 'Italic', exact: true})).toBeInViewport();
  await page.locator('#formatting-expand').click();
  await expect(page.getByRole('combobox', {name: 'Insert block', exact: true})).toBeInViewport();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.locator('#format-source').click();
  await fillSource(page, 'Exact mobile draft');
  await page.locator('#format-visual').click();
  await expect(page.locator('#editor')).toContainText('Exact mobile draft');
  await page.locator('#format-source').click();
  await expectEditorText(page, 'Exact mobile draft');
});
test('folders navigate in the library and breadcrumbs preserve nested locations on reload', async ({page}) => {
  await app.api('/p/default/api/note', {path: 'Nested/Leaf/example.md', content: '# Hierarchy', revision: ''});
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#sidebar [data-folder]')).toHaveCount(0);
  await page
    .locator('#folder-groups .browse-item-link')
    .filter({hasText: /Nested/})
    .click();
  await page.locator('#folder-groups .browse-item-link').filter({hasText: /Leaf/}).click();
  await expect(page.locator('#folder-title')).toHaveText('Leaf');
  await page.reload();
  await expect(page.locator('#folder-title')).toHaveText('Leaf');
  await page.locator('#folder-crumbs').getByRole('link', {name: 'Nested', exact: true}).click();
  await expect(page.locator('#folder-title')).toHaveText('Nested');
});
test('phone filename editing gives the input room and returns to the compact reader', async ({page}) => {
  await page.setViewportSize({width: 320, height: 700});
  await page.goto(app.origin + '/?project=default&note=Garden%2Ffield-notes.md');
  await page.locator('#note-details-toggle').click();
  await page.locator('#filename-open').click();
  expect((await page.locator('#filename-input').boundingBox()).width).toBeGreaterThan(130);
  await expect(page.locator('#filename-cancel')).toBeInViewport();
  await page.locator('#filename-cancel').click();
  expect((await page.locator('#main > header').boundingBox()).height).toBeLessThan(90);
  await expect(page.locator('#path')).toHaveText('A small garden of ideas');
});
