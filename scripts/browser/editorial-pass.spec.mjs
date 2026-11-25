import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {caretAtLineEnd} from './editor-text.mjs';

// Visual contracts from the editorial review: one alignment grid, one quiet
// button language and no developer-facing copy in the primary surfaces.
let app, cookies;
const account = {username: 'editorial-admin', password: 'DisposableEditorial123!'};
const protocol = `---
tags: [method, water]
status: Active
---
# Sampling protocol

Every sample we take should be repeatable by someone who has never visited the site.

## Before you leave

Check the tide table and plan to arrive early.

## At the site

| Site | Depth (m) | Bottles |
|---|---|---|
| Mill Creek | 0.5 | 3 |
| North Flats | 1.2 | 12 |
`;
test.beforeAll(async ({browser}) => {
  app = await startApp({name: 'forgetmenote-editorial-', account});
  await app.api('/api/projects', {id: 'estuary', name: 'Estuary Monitoring'});
  const notes = {
    'Field guide/Sampling protocol.md': protocol,
    'Field guide/Equipment checklist.md': '# Equipment checklist\n\nWhat goes in the van.',
    'Sites/Mill Creek.md': '# Mill Creek\n\nTidal creek below the old mill.',
    'Weekly notes/2026-W40.md': '# Week 40\n\nDuplicate samples taken at Mill Creek.',
    'Reading list.md': '---\ntags: [reading]\n---\n# Reading list\n\nPapers worth reading.',
    'Open questions.md': '---\nstatus: Draft\n---\n# Open questions\n\nThings we have not decided yet.'
  };
  for (const [path, content] of Object.entries(notes))
    await app.api('/p/estuary/api/note', {path, content, revision: ''});
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
  await page.setViewportSize({width: 1440, height: 900});
});
const note = path => app.origin + '/?project=estuary&note=' + encodeURIComponent(path);
const library = () => app.origin + '/?project=estuary&folder=';
const style = (locator, property) => locator.evaluate((element, name) => getComputedStyle(element)[name], property);
const accent = page =>
  page.evaluate(() => {
    const probe = document.createElement('span');
    probe.style.color = 'var(--accent)';
    document.body.append(probe);
    const color = getComputedStyle(probe).color;
    probe.remove();
    return color;
  });
async function openSearch(page, query) {
  await page.goto(library());
  await expect(page.locator('#search-open')).toBeEnabled();
  await page.locator('#search-open').click();
  if (query) {
    await page.locator('#search').fill(query);
    await expect(page.locator('#search-status')).toHaveText(/result/);
  }
}

test('sidebar keeps the notebook name whole, quiets creation and shows one active destination', async ({page}) => {
  await page.goto(library());
  await expect(page.locator('#folder-title')).toHaveText('Estuary Monitoring');
  const fits = await page.locator('#project-select').evaluate(select => {
    const s = getComputedStyle(select),
      canvas = document.createElement('canvas').getContext('2d');
    canvas.font = `${s.fontWeight} ${s.fontSize} ${s.fontFamily}`;
    return (
      select.clientWidth - parseFloat(s.paddingLeft) - parseFloat(s.paddingRight) >=
      canvas.measureText(select.selectedOptions[0].text).width
    );
  });
  expect(fits).toBe(true);
  await expect(page.locator('#notebook-create')).toBeHidden();
  await page.getByRole('button', {name: 'Add notebook', exact: true}).click();
  await expect(page.locator('#notebook-create')).toBeVisible();
  await expect(page.locator('#notebook-import')).toBeVisible();
  await page.keyboard.press('Escape');
  expect(await style(page.locator('#sidebar-new-note'), 'backgroundColor')).not.toBe(await accent(page));
  await page.locator('#search-open').click();
  await expect(page.locator('#search')).toBeFocused();
  await expect(page.locator('#browse-root')).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)');
  await expect(page.locator('#search-open')).not.toHaveCSS('background-color', 'rgba(0, 0, 0, 0)');
});

test('account menu aligns every action with a distinct icon and separates sign out', async ({page}) => {
  await page.goto(library());
  await page.locator('#account-actions-toggle').click();
  const actions = page.locator('#account-actions :is(button,a)');
  await expect(actions.first()).toBeVisible();
  const items = await actions.evaluateAll(nodes =>
    nodes.map(node => ({
      icon: node.querySelector('svg path')?.getAttribute('d') || '',
      text: node.textContent.trim(),
      x: (() => {
        const range = document.createRange();
        range.selectNodeContents(node.lastChild);
        return Math.round(range.getBoundingClientRect().x);
      })()
    }))
  );
  expect(items.every(item => item.icon)).toBe(true);
  expect(new Set(items.map(item => item.icon)).size).toBe(items.length);
  expect(new Set(items.map(item => item.x)).size).toBe(1);
  expect(
    await page.locator('#account-actions form').evaluate(form => parseFloat(getComputedStyle(form).borderTopWidth))
  ).toBeGreaterThan(0);
});

test('search keeps the query first, scope compact and results in the same column', async ({page}) => {
  await openSearch(page, 'sample');
  await expect(page.locator('#search-close')).toHaveText('Back');
  await expect(page.locator('#search-status')).toHaveText('2 results');
  const field = await page.locator('#search').boundingBox(),
    first = await page.locator('#search-results li').first().boundingBox();
  expect(first.y - (field.y + field.height)).toBeLessThan(150);
  const list = await page.locator('#search-results').boundingBox(),
    row = await page.locator('.search-row').boundingBox();
  expect(Math.abs(list.x - row.x)).toBeLessThanOrEqual(1);
  expect(list.x + list.width).toBeLessThanOrEqual(row.x + row.width + 1);
  const scopes = await page
    .locator('#search-projects label')
    .evaluateAll(labels => labels.map(label => label.getBoundingClientRect().height));
  expect(Math.max(...scopes)).toBeLessThan(40);
  expect(await style(page.locator('#search-corpus'), 'appearance')).toBe('none');
  expect(await style(page.locator('#search-projects input').first(), 'accentColor')).toBe(await accent(page));
  await expect(page.locator('#search-results mark').first()).toHaveText(/sample/i);
  await expect(
    page.locator('#search-results li').filter({hasText: 'Sampling protocol'}).locator('.note-filename')
  ).toHaveText('Field guide');
  await page.locator('#search-options-toggle').click();
  await expect(page.locator('#note-filters')).toBeVisible();
  expect(await style(page.locator('#advanced-search > summary'), 'listStyleType')).toBe('none');
  const filters = await page.locator('#note-filters').boundingBox(),
    status = await page.locator('#search-status').boundingBox();
  expect(status.y - (filters.y + filters.height)).toBeGreaterThanOrEqual(12);
});

test('note top bar uses one quiet button style, a folder breadcrumb and no idle status', async ({page}) => {
  await page.goto(note('Field guide/Sampling protocol.md'));
  await expect(page.locator('#preview h1')).toHaveText('Sampling protocol');
  await expect(page.locator('#mode')).toBeEnabled();
  const buttons = ['#note-back', '#chat-open', '#note-context-toggle', '#mode', '#note-details-toggle'];
  const looks = await Promise.all(
    buttons.map(selector =>
      page.locator(selector).evaluate(node => {
        const s = getComputedStyle(node);
        return s.borderTopColor + '|' + s.backgroundColor;
      })
    )
  );
  expect(new Set(looks).size).toBe(1);
  expect(looks[0]).toContain('rgba(0, 0, 0, 0)|rgba(0, 0, 0, 0)');
  await expect(page.locator('#note-folder')).toHaveText('Field guide');
  await expect(page.locator('#note-path')).toHaveText('Field guide/Sampling protocol.md');
  expect((await page.locator('#note-path').boundingBox())?.width ?? 0).toBeLessThanOrEqual(1);
  await expect(page.locator('#save-state')).toBeHidden();
  await expect(page.locator('#note-context-toggle')).toHaveText('Context');
});

test('reading surface aligns metadata with the text, sets tables quietly and lists sections only', async ({page}) => {
  await page.goto(note('Field guide/Sampling protocol.md'));
  await expect(page.locator('#metadata-bar')).toBeVisible();
  const chip = await page.locator('#metadata-bar > *').first().boundingBox(),
    text = await page.locator('#preview p').first().boundingBox();
  expect(Math.abs(chip.x - text.x)).toBeLessThanOrEqual(2);
  expect(
    await page
      .locator('#metadata-bar > *')
      .evaluateAll(nodes => nodes.every(node => getComputedStyle(node).borderTopWidth === '0px'))
  ).toBe(true);
  const header = page.locator('#preview th').first();
  expect(await style(header, 'textAlign')).toMatch(/^(left|start)$/);
  expect(await style(header, 'backgroundColor')).toBe('rgba(0, 0, 0, 0)');
  expect(await style(page.locator('#preview td').first(), 'borderLeftWidth')).toBe('0px');
  expect(await style(page.locator('#preview tbody tr').first().locator('td').nth(1), 'textAlign')).toBe('right');
  expect(await style(page.locator('#preview th').nth(1), 'textAlign')).toBe('right');
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('#note-context-contents')).toBeVisible();
  await expect(page.locator('#note-context-contents a')).toHaveText(['Before you leave', 'At the site']);
});

test('note menu has no stray divider, distinct icons and no duplicated destinations', async ({page}) => {
  await page.goto(note('Field guide/Sampling protocol.md'));
  await expect(page.locator('#note-details-toggle')).toBeEnabled();
  await page.locator('#note-details-toggle').click();
  await expect(page.locator('#note-details-actions')).toBeVisible();
  const items = page.locator('#note-details-actions > button:visible');
  expect(await style(items.first(), 'borderTopWidth')).toBe('0px');
  // Contents and search have one home each: the Context button and the sidebar.
  for (const id of ['#contents-open', '#note-search']) await expect(page.locator(id)).toHaveCount(0);
  const icons = await items.evaluateAll(nodes =>
    nodes.map(node => ({
      icon: node.querySelector('svg')?.innerHTML || '',
      x: Math.round(node.querySelector('svg')?.getBoundingClientRect().x || 0)
    }))
  );
  expect(new Set(icons.map(item => item.icon)).size).toBe(icons.length);
  expect(new Set(icons.map(item => item.x)).size).toBe(1);
});

test('editing keeps one toolbar row, chips out of the way and source text in the reading column', async ({page}) => {
  await page.goto(note('Field guide/Sampling protocol.md'));
  await page.locator('#mode').click();
  await expect(page.locator('#editor')).toHaveAttribute('aria-label', 'Note content');
  await expect(page.locator('#metadata-bar')).toBeHidden();
  const visualTools = await page.locator('#editor-tools').boundingBox();
  expect(visualTools.height).toBeLessThanOrEqual(56);
  // Visual text sits in the reading column: the content box of its lines.
  const prose = await page
    .locator('#editor .cm-line')
    .filter({hasText: /\w/})
    .last()
    .evaluate(node => {
      const r = node.getBoundingClientRect(),
        s = getComputedStyle(node);
      return {
        x: r.x + parseFloat(s.paddingLeft),
        width: r.width - parseFloat(s.paddingLeft) - parseFloat(s.paddingRight)
      };
    });
  expect(await style(page.locator('#save'), 'opacity')).toBe('1');
  await page.locator('#format-source').click();
  await expect(page.locator('#editor')).toBeVisible();
  const sourceTools = await page.locator('#editor-tools').boundingBox();
  expect(Math.abs(sourceTools.height - visualTools.height)).toBeLessThanOrEqual(2);
  const insert = await page.getByRole('combobox', {name: 'Insert block', exact: true}).boundingBox(),
    format = await page.locator('.editor-format').boundingBox();
  expect(insert.x).toBeGreaterThanOrEqual(format.x + format.width);
  const editor = await page.locator('#editor').evaluate(node => {
    const r = node.getBoundingClientRect(),
      s = getComputedStyle(node);
    return {
      left: r.x + parseFloat(s.paddingLeft),
      width: r.width - parseFloat(s.paddingLeft) - parseFloat(s.paddingRight)
    };
  });
  expect(Math.abs(editor.left - prose.x)).toBeLessThanOrEqual(8);
  expect(editor.width).toBeLessThanOrEqual(prose.width + 8);
});

test('library rows stay compact, folders follow card view and the notebook map is styled', async ({page}) => {
  await page.goto(library());
  await expect(page.locator('#folder-count')).toHaveText('2 notes · 3 folders');
  const folder = await page.locator('#folder-groups .browse-item').first().boundingBox();
  expect(folder.height).toBeLessThanOrEqual(64);
  await page.locator('[data-browse-view="cards"]').click();
  expect((await page.locator('#folder-groups .browse-item').first().boundingBox()).width).toBeLessThan(420);
  await page.locator('[data-browse-view="list"]').click();
  const map = page.locator('#notebook-outline > summary');
  if (await map.isVisible()) expect(await style(map, 'listStyleType')).toBe('none');
});

test('phone overlay dims the whole page, hides shortcuts and keeps one reader bar style', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(note('Field guide/Sampling protocol.md'));
  await expect(page.locator('#reader-actions')).toBeVisible();
  const looks = await Promise.all(
    ['#mode', '#chat-open', '#note-context-toggle', '#note-details-toggle'].map(selector =>
      page.locator(selector).evaluate(node => getComputedStyle(node).borderTopColor)
    )
  );
  expect(new Set(looks).size).toBe(1);
  await page.locator('#menu').click();
  await expect(page.locator('#sidebar')).toBeVisible();
  await expect(page.locator('#search-open kbd')).toBeHidden();
  const covered = await page.evaluate(() => {
    const header = document.querySelector('#main > header').getBoundingClientRect();
    return document.elementFromPoint(header.right - 8, header.top + header.height / 2)?.id;
  });
  expect(covered).toBe('sidebar-backdrop');
});

test('search on phones keeps notebook scope compact', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(library());
  await page.locator('[data-destination="search-open"]').click();
  await page.locator('#search').fill('sample');
  await expect(page.locator('#search-status')).toHaveText('2 results');
  const scopes = await page
    .locator('#search-projects label')
    .evaluateAll(labels => labels.map(label => label.getBoundingClientRect().height));
  expect(Math.max(...scopes)).toBeLessThan(48);
  const field = await page.locator('#search').boundingBox(),
    first = await page.locator('#search-results li').first().boundingBox();
  expect(first.y - (field.y + field.height)).toBeLessThan(220);
});

const shared = `# Shared rendering

> A quoted reminder.

| Site | Depth (m) | Bottles | Notes |
|---|---|---|---|
| Mill Creek | 0.5 | 3 | Access from the boat ramp |
| North Flats | 1.2 | 12 | Soft mud, wear waders |

Inline values like \`site_id\` must match the register.
`;
async function tableLayout(table) {
  return table.evaluate(node =>
    [...node.querySelector('tr').children].map(cell => ({
      width: Math.round(cell.getBoundingClientRect().width),
      align: getComputedStyle(cell).textAlign
    }))
  );
}

test('reading and editing share table columns, quotes and inline code', async ({page}) => {
  await app.api('/p/estuary/api/note', {path: 'Field guide/Shared rendering.md', content: shared, revision: ''});
  await page.goto(note('Field guide/Shared rendering.md'));
  await expect(page.locator('#preview table')).toBeVisible();
  const read = {
    table: await tableLayout(page.locator('#preview table')),
    quote: await style(page.locator('#preview blockquote'), 'fontStyle'),
    code: await page.locator('#preview code').evaluate(node => {
      const s = getComputedStyle(node);
      return [s.color, s.backgroundColor, s.fontSize].join(' ');
    }),
    strong: await style(page.locator('#preview th').first(), 'fontWeight')
  };
  expect(read.table.map(cell => cell.align)).toEqual(['left', 'right', 'right', 'left']);
  await page.locator('#mode').click();
  await expect(page.locator('#editor .cm-lp-table')).toBeVisible();
  const edited = {
    table: await tableLayout(page.locator('#editor .cm-lp-table')),
    quote: await style(page.locator('#editor .cm-lp-quote').first(), 'fontStyle'),
    code: await page
      .locator('#editor .cm-lp-inline-code')
      .first()
      .evaluate(node => {
        const s = getComputedStyle(node);
        return [s.color, s.backgroundColor, s.fontSize].join(' ');
      }),
    strong: await style(page.locator('#editor .cm-lp-table th').first(), 'fontWeight')
  };
  expect(edited.table.map(cell => cell.align)).toEqual(read.table.map(cell => cell.align));
  for (const [index, cell] of edited.table.entries())
    expect(Math.abs(cell.width - read.table[index].width), 'column ' + index + ' width').toBeLessThanOrEqual(2);
  expect(edited.quote).toBe(read.quote);
  expect(edited.code).toBe(read.code);
  expect(edited.strong).toBe(read.strong);
});

test('Save is quiet and disabled while the note is saved and primary once it changes', async ({page}) => {
  await app.api('/p/estuary/api/note', {
    path: 'Field guide/Save state.md',
    content: '# Save state\n\nFirst line.\n',
    revision: ''
  });
  await page.goto(note('Field guide/Save state.md'));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expect(page.locator('#save')).toBeVisible();
  await expect(page.locator('#save')).toBeDisabled();
  expect(await style(page.locator('#save'), 'borderTopColor')).toBe('rgba(0, 0, 0, 0)');
  expect(await style(page.locator('#save'), 'backgroundColor')).toBe('rgba(0, 0, 0, 0)');
  await caretAtLineEnd(page, 'First line.');
  await page.keyboard.type(' More.');
  await expect(page.locator('#save')).toBeEnabled();
  expect(await style(page.locator('#save'), 'backgroundColor')).toBe(await accent(page));
  await page.locator('#save').click();
  await expect(page.locator('#save')).toBeDisabled();
});

test('a phone note keeps a short status, aligned properties and four even icon tabs', async ({page, browser}) => {
  await app.api('/p/estuary/api/note', {
    path: 'Field guide/Phone status.md',
    content: '---\ntags: [method]\nstatus: Active\n---\n# Phone status\n\nFirst line.\n',
    revision: ''
  });
  // Another session's unsaved live change gives the note its longest routine status.
  const writer = await browser.newContext({viewport: {width: 1440, height: 900}});
  try {
    await writer.addCookies(cookies);
    const desk = await writer.newPage();
    await desk.goto(note('Field guide/Phone status.md'));
    await expect(desk.locator('#document')).toHaveAttribute('data-live', 'connected');
    await desk.locator('#mode').click();
    await caretAtLineEnd(desk, 'First line.');
    await desk.keyboard.type(' Pending.');
    await expect(desk.locator('#save-state')).toContainText('Save to create a version');
  } finally {
    await writer.close();
  }
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(note('Field guide/Phone status.md'));
  await expect(page.locator('#reader-actions')).toBeVisible();
  // Read mode has no Save button, so the status points to Edit.
  await expect(page.locator('#save-state')).toHaveText('Live changes synced · Edit to save a version');
  // The reading view renders the live text again once the session starts; the
  // layout is measured after that, not while it replaces the saved text.
  await expect(page.locator('#preview')).toContainText('First line. Pending.');
  const status = await page.locator('#save-state').evaluate(node => {
    const after = getComputedStyle(node, '::after'),
      box = node.getBoundingClientRect(),
      header = node.closest('header').getBoundingClientRect();
    return {content: after.content, height: box.height, right: box.right, headerRight: header.right};
  });
  expect(status.content).toBe('"Live changes synced"');
  expect(status.height).toBeLessThan(24);
  expect(status.right).toBeLessThanOrEqual(status.headerRight);
  const properties = await page.locator('#metadata-bar > *').first().boundingBox(),
    title = await page.locator('#preview h1').boundingBox();
  expect(Math.abs(properties.x - title.x)).toBeLessThanOrEqual(1);
  const tabs = await page.locator('#reader-actions').evaluate(bar =>
    ['#mode', '#chat-open', '#note-context-toggle', '#note-details-toggle'].map(selector => {
      const tab = bar.querySelector(selector),
        glyph = tab.querySelector(':scope > .ui-icon');
      return {
        width: Math.round(tab.getBoundingClientRect().width),
        icon: !!glyph && glyph.getBoundingClientRect().width > 0
      };
    })
  );
  expect(tabs.every(tab => tab.icon)).toBe(true);
  expect(Math.max(...tabs.map(tab => tab.width)) - Math.min(...tabs.map(tab => tab.width))).toBeLessThanOrEqual(1);
});

test('focus mode toggles read as switches and sliders show their values', async ({page}) => {
  await page.goto(note('Field guide/Sampling protocol.md'));
  await page.locator('#focus-toggle').click();
  await expect(page.locator('body')).toHaveClass(/focus-mode/);
  // The toolbar waits below the bottom edge until the pointer reaches it.
  await page.mouse.move(720, 898);
  await expect(page.locator('#main > header')).toHaveAttribute('data-revealed', '');
  await expect(page.locator('#focus-text-size + span .focus-value')).toHaveText('100%');
  await expect(page.locator('#focus-page-position + span .focus-value')).toHaveText('Centred');
  await page.locator('#focus-text-size').fill('120');
  await expect(page.locator('#focus-text-size + span .focus-value')).toHaveText('120%');
  const off = await page
    .locator('#focus-typewriter')
    .evaluate(node => getComputedStyle(node, '::before').borderTopColor);
  await page.locator('#focus-typewriter').click();
  await expect(page.locator('#focus-typewriter')).toHaveAttribute('aria-pressed', 'true');
  expect(
    await page.locator('#focus-typewriter').evaluate(node => getComputedStyle(node, '::before').borderTopColor)
  ).not.toBe(off);
  await page.locator('#focus-typewriter').click();
  await page.locator('#focus-text-size').fill('100');
  await page.locator('#focus-toggle').click();
});

test('search titles read as a breadcrumb and excerpts skip the title and mark whole words', async ({page}) => {
  await openSearch(page, 'sample');
  await expect(page.locator('#search-results-title')).toHaveText('Estuary Monitoring/Search');
  const protocol = page.locator('#search-results li').filter({hasText: 'Sampling protocol'}).locator('.search-excerpt');
  await expect(protocol).toHaveText(/^Every sample we take/);
  await expect(
    page.locator('#search-results li').filter({hasText: 'Week 40'}).locator('.search-excerpt mark')
  ).toHaveText('samples');
});

test('library tags align right and Draft reads as a status', async ({page}) => {
  await page.goto(library());
  const row = page.locator('#folder-items .browse-item').filter({hasText: 'Open questions'});
  const link = await row.locator('.browse-item-link').boundingBox(),
    tags = await row.locator('.card-tags').boundingBox(),
    last = await row.locator('.card-tags span').last().boundingBox();
  expect(link.x + link.width - (last.x + last.width)).toBeLessThanOrEqual(64);
  expect(tags.x + tags.width).toBeLessThanOrEqual(link.x + link.width);
  expect(await row.locator('.card-status').evaluate(status => getComputedStyle(status, '::before').borderRadius)).toBe(
    '50%'
  );
});

test('empty Trash and notifications show one clear empty state with the close in the header', async ({page}) => {
  await page.goto(library());
  await page.locator('#trash-open').click();
  await expect(page.locator('#trash-list')).toHaveText('Trash is empty.');
  await expect(page.locator('#trash-reload')).toBeHidden();
  await expect(page.locator('#trash-dialog .dialog-title #trash-close')).toBeVisible();
  expect(await style(page.locator('#trash-list li'), 'borderBottomWidth')).toBe('0px');
  await page.locator('#trash-close').click();
  await page.locator('#notifications-open').click();
  const dialog = page.locator('#notifications-dialog');
  await expect(dialog).toContainText('No notifications.');
  await expect(dialog).not.toContainText('Showing');
  // The sticky heading shares the dialog's surface instead of a grey band.
  expect(await style(dialog.locator('.dialog-title'), 'backgroundColor')).toBe(await style(dialog, 'backgroundColor'));
});

test('new note fields use plain words and matching controls', async ({page}) => {
  await page.goto(library());
  await page.locator('#new-note').click();
  const folder = page.getByRole('combobox', {name: 'Folder', exact: true});
  await expect(folder).toHaveValue('Top level');
  await expect(page.getByLabel('Save to')).toHaveValue('');
  await expect(page.locator('#new-container option')).toHaveText(['Notes', 'Drafts']);
  const field = await folder.boundingBox(),
    toggle = await page.locator('#new-folder-toggle').boundingBox(),
    select = await page.locator('#new-container').boundingBox();
  // The toggle sits inside the field, which is as wide as the select below it.
  expect(toggle.x + toggle.width).toBeLessThanOrEqual(field.x + field.width);
  expect(Math.abs(field.width - select.width)).toBeLessThanOrEqual(1);
  await page.locator('#new-folder-toggle').click();
  await expect(page.locator('#new-folder-options')).toBeVisible();
});

test('chat without a connection offers setup instead of an inert composer', async ({page}) => {
  await page.goto(note('Field guide/Sampling protocol.md'));
  await page.locator('#chat-open').click();
  await expect(page.locator('#chat-message')).toContainText('not configured');
  await expect(page.locator('#chat-input')).toBeDisabled();
  await expect(page.locator('#chat-input')).toHaveAttribute('placeholder', 'Chat is not set up yet');
  await expect(page.locator('#chat-mode')).toBeDisabled();
  await expect(page.locator('#chat-send')).toBeDisabled();
  await expect(page.locator('#chat-new')).toBeHidden();
  await expect(page.locator('#chat-refresh')).toBeVisible();
  const setup = page.getByRole('link', {name: 'Set up chat'});
  await expect(setup).toHaveAttribute('href', /page=administration&category=ai/);
  // The panel's close control is a quiet icon, not the only filled button.
  expect(await style(page.locator('#chat-panel #chat-open'), 'backgroundColor')).toBe('rgba(0, 0, 0, 0)');
  await setup.click();
  await expect(page.locator('#settings-page')).toBeVisible();
});
