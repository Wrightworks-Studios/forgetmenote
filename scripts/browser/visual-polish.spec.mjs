import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app, service, cookies;
test.beforeAll(async ({browser}) => {
  app = await startApp({
    name: 'fmn-visual-polish-',
    account: {username: 'visual-admin', password: 'DisposableVisual123!'}
  });
  await app.api('/p/default/api/note', {
    path: 'Welcome.md',
    content: '# Welcome\n\nA small notebook for exploring ideas.',
    revision: ''
  });
  const directory = await app.api('/api/admin'),
    admin = directory.users.find(user => user.admin && user.id !== '@local-admin');
  service = await app.api('/api/organization/action/create_service_account', {
    name: 'Visual test worker',
    steward: admin.id
  });
  await app.api('/api/members', {project: 'default', user: service.id, role: 'edit'});
  const context = await browser.newContext();
  try {
    await app.login(await context.newPage(), 'password');
    cookies = await context.cookies();
  } finally {
    await context.close();
  }
});
test.beforeEach(async ({context}) => {
  await context.addCookies(cookies);
});

for (const schedule of [
  {mode: 'once', label: 'Run at', value: '2099-01-01T12:00'},
  {mode: 'interval', label: 'Interval (seconds)', value: '3600'}
])
  test(`new automation keeps the ${schedule.mode} trigger while completing its fields`, async ({page}) => {
    await page.goto(app.origin);
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await page.locator('#admin-dialog [data-settings-category=automation]').click();
    const root = page.locator('#integration-jobs');
    await root.getByRole('button', {name: 'New automation', exact: true}).click();
    await root.getByLabel('Trigger', {exact: true}).selectOption(schedule.mode);
    await expect(root.getByLabel(schedule.label, {exact: true})).toBeVisible();
    await root.getByLabel('ID', {exact: true}).fill('visual-' + schedule.mode);
    await root.getByLabel('Service account', {exact: true}).selectOption(service.id);
    await root.getByLabel('Notebook', {exact: true}).selectOption('default');
    await root.getByLabel(schedule.label, {exact: true}).fill(schedule.value);
    await root.getByRole('button', {name: 'Save automation', exact: true}).click();
    const card = root.locator('[data-automation="visual-' + schedule.mode + '"]');
    await expect(card).toContainText('Paused');
    await card.getByRole('button', {name: 'Edit', exact: true}).click();
    await expect(root.getByLabel('Trigger', {exact: true})).toHaveValue(schedule.mode);
    await expect(root.getByLabel(schedule.label, {exact: true})).toHaveValue(schedule.value);
  });
test.afterAll(async () => {
  await app?.close();
});
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

test('an unfinished source event cannot silently become a manual automation', async ({page}) => {
  await page.goto(app.origin);
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await page.locator('#admin-dialog [data-settings-category=automation]').click();
  const root = page.locator('#integration-jobs');
  await root.getByRole('button', {name: 'New automation', exact: true}).click();
  await root.getByLabel('Task', {exact: true}).selectOption('learning');
  await expect(root.getByLabel('Trigger', {exact: true})).toHaveValue('usage');
  await root.getByLabel('Task', {exact: true}).selectOption('native');
  await expect(root.getByLabel('Trigger', {exact: true})).toHaveValue('manual');
  await root.getByLabel('ID', {exact: true}).fill('visual-source-event');
  await root.getByLabel('Service account', {exact: true}).selectOption(service.id);
  await root.getByLabel('Notebook', {exact: true}).selectOption('default');
  await root.getByLabel('Trigger', {exact: true}).selectOption('event');
  await expect(root.getByLabel('Trigger', {exact: true})).toHaveValue('event');
  await expect(root.getByLabel('Automation JSON')).toBeVisible();
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(page.locator('#admin-status')).toContainText('invalid event debounce');
  await expect(root.getByLabel('Trigger', {exact: true})).toHaveValue('event');
});

for (const width of [1440, 390])
  test(`account menu offers consistent action rows at ${width}px`, async ({page}) => {
    await page.setViewportSize({width, height: 900});
    await page.goto(app.origin);
    await expect(page.locator('#account-name')).toHaveText('visual-admin');
    if (width < 720) await page.locator('#menu').click();
    await page.locator('#account-actions-toggle').click();
    const menu = page.locator('#account-actions'),
      connect = menu.getByRole('link', {name: 'Connect your AI'});
    await expect(connect).toBeVisible();
    const bounds = await menu.evaluate(element => ({
      menu: element.getBoundingClientRect().toJSON(),
      sidebar: document.getElementById('sidebar').getBoundingClientRect().toJSON()
    }));
    expect(bounds.menu.left, 'the menu must fit inside the sidebar').toBeGreaterThanOrEqual(bounds.sidebar.left);
    expect(bounds.menu.right, 'the menu border must not be clipped by the sidebar').toBeLessThanOrEqual(
      bounds.sidebar.right
    );
    const rows = await menu.locator('button,a').evaluateAll(items =>
      items.map(item => {
        const box = item.getBoundingClientRect(),
          style = getComputedStyle(item);
        return {
          label: item.textContent.trim(),
          x: box.x,
          width: box.width,
          height: box.height,
          color: style.color,
          decoration: style.textDecorationLine
        };
      })
    );
    const reference = rows.find(row => row.label === 'Your settings');
    for (const row of rows) {
      expect(row.height, row.label + ' needs a compact mouse target').toBeGreaterThanOrEqual(32);
      expect(row.height, row.label + ' should retain compact mouse spacing').toBeLessThanOrEqual(36);
      expect(row.width, row.label + ' should span the menu').toBeCloseTo(reference.width, 0);
      expect(row.x, row.label + ' should align with adjacent actions').toBeCloseTo(reference.x, 0);
      expect(row.color, row.label + ' should share the action text color').toBe(reference.color);
      expect(row.decoration, row.label + ' should look like a menu action').toBe('none');
    }
    await connect.focus();
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(app.origin + '/account/connect');
    await expect(page.getByRole('heading', {name: 'Connect your AI', exact: true})).toBeVisible();
  });

test('editor actions have visible icons and remain within a narrow toolbar', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Welcome.md');
  await expect(page.locator('#mode')).toBeEnabled();
  await page.locator('#mode').click();
  for (const width of [1440, 390, 320]) {
    await page.setViewportSize({width, height: 900});
    for (const id of ['notebook-link-insert', 'writing-open']) {
      const geometry = await page.locator('#' + id).evaluate(button => {
        const icon = button.querySelector('svg'),
          path = icon.querySelector('path').getBBox(),
          box = button.getBoundingClientRect();
        return {pathWidth: path.width, pathHeight: path.height, right: box.right, left: box.left, viewport: innerWidth};
      });
      expect(geometry.pathWidth, id + ' icon must be drawn').toBeGreaterThan(0);
      expect(geometry.pathHeight, id + ' icon must be drawn').toBeGreaterThan(0);
      expect(geometry.left).toBeGreaterThanOrEqual(0);
      expect(geometry.right).toBeLessThanOrEqual(geometry.viewport);
    }
  }
  await page.locator('#mode').click();
  await page.locator('#note-details-toggle').click();
  await expect(page.locator('#protection-label')).toBeVisible();
  await expect(page.locator('#protection-label')).toHaveText('Lock…');
});

test('the formatting toolbar draws every icon and fits every width in every mode', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Welcome.md');
  await expect(page.locator('#mode')).toBeEnabled();
  await page.locator('#mode').click();
  const bar = page.getByRole('group', {name: 'Formatting toolbar', exact: true});
  for (const width of [1440, 1024, 900, 390, 320]) {
    await page.setViewportSize({width, height: 900});
    for (const format of width >= 900 ? ['visual', 'source', 'split'] : ['visual', 'source']) {
      await page.locator('#format-' + format).click();
      await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
      await expect(bar).toBeVisible();
      const geometry = await bar.evaluate(toolbar => ({
        icons: [...toolbar.querySelectorAll('button')].map(button => {
          const box = button.querySelector('svg path').getBBox();
          return {name: button.getAttribute('aria-label'), drawn: box.width > 0 && box.height > 0};
        }),
        right: document.getElementById('formatting-tools').getBoundingClientRect().right,
        page: document.documentElement.scrollWidth,
        viewport: innerWidth
      }));
      for (const icon of geometry.icons) expect(icon.drawn, icon.name + ' icon must be drawn').toBe(true);
      expect(geometry.right, format + ' toolbar at ' + width).toBeLessThanOrEqual(geometry.viewport + 1);
      expect(geometry.page, format + ' page width at ' + width).toBeLessThanOrEqual(geometry.viewport);
      if (width >= 900) {
        for (const name of ['Undo', 'Bold', 'Link', 'Task list', 'Comment on selection'])
          await expect(bar.getByRole('button', {name, exact: true})).toBeInViewport({ratio: 1});
        await expect(bar.getByRole('combobox', {name: 'Insert block', exact: true})).toBeInViewport({ratio: 1});
      } else {
        // A phone shows the formatting buttons first and the rest on request.
        await expect(bar.getByRole('button', {name: 'Bold', exact: true})).toBeInViewport();
        const more = page.locator('#formatting-expand');
        await expect(more).toBeVisible();
        await more.click();
        await expect(bar.getByRole('combobox', {name: 'Insert block', exact: true})).toBeInViewport();
        await expect(bar.getByRole('button', {name: 'Comment on selection', exact: true})).toBeInViewport();
        await more.click();
        await expect(more).toHaveText('More tools');
      }
    }
  }
});

test('multiline dialog fields share the form control appearance', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.locator('#create-toggle').click();
  await page.locator('#quicknotes-open').click();
  await expect(page.locator('#quicknotes-text')).toBeVisible();
  for (const theme of ['light', 'dark', 'parchment']) {
    await page.evaluate(value => (document.documentElement.dataset.theme = value), theme);
    const styles = await page.locator('#quicknotes-text').evaluate(field => {
      const control = getComputedStyle(document.getElementById('search')),
        style = getComputedStyle(field);
      return {
        radius: style.borderRadius,
        referenceRadius: control.borderRadius,
        padding: parseFloat(style.paddingLeft),
        color: style.color,
        referenceColor: control.color
      };
    });
    expect(styles.radius).toBe(styles.referenceRadius);
    expect(styles.padding).toBeGreaterThanOrEqual(10);
    expect(styles.color).toBe(styles.referenceColor);
  }
});

test('notification errors replace loading and allow an explicit recovery', async ({page}) => {
  await page.goto(app.origin);
  await page.route('**/api/notifications*', route =>
    route.fulfill({status: 503, json: {error: 'Notification service unavailable'}})
  );
  await page.locator('#notifications-open').click();
  const dialog = page.locator('#notifications-dialog');
  await expect(dialog.getByRole('alert')).toContainText('Notification service unavailable');
  await expect(dialog).not.toContainText('Loading…');
  await page.unroute('**/api/notifications*');
  await dialog.getByRole('button', {name: 'Try again', exact: true}).click();
  await expect(dialog).toContainText('No notifications.');
  await expect(dialog.getByRole('alert')).toBeHidden();
});

test('advanced search uses page scrolling rather than clipping its controls', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#search-open')).toBeEnabled();
  await page.locator('#search-open').click();
  await page.setViewportSize({width: 320, height: 700});
  await page.locator('#search-options-toggle').click();
  await page.locator('#advanced-search summary').click();
  const panel = page.locator('#note-filters'),
    geometry = await panel.evaluate(element => ({scroll: element.scrollHeight, height: element.clientHeight}));
  expect(geometry.scroll).toBeLessThanOrEqual(geometry.height + 1);
  await page.locator('#search-drafts').scrollIntoViewIfNeeded();
  await expect(page.locator('#search-drafts')).toBeInViewport();
});

test('draft reconciliation keeps separate action targets on a phone', async ({page}) => {
  const path = 'Retained.md',
    note = await app.api('/p/default/api/note', {path, content: '# Shared version\n', revision: ''});
  await page.goto(app.origin + '/login');
  const user = await (await page.request.get(app.origin + '/api/me')).json();
  await page.evaluate(
    ({scope, note}) =>
      localStorage.setItem(
        'gitnotes:draft:' + scope,
        JSON.stringify({content: '# Private draft\n', revision: note.revision, version: note.version, private: true})
      ),
    {scope: user.id + '/default/' + path, note}
  );
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(app.origin + '/?project=default&note=' + path);
  await expect(page.locator('#save-state')).toContainText('recovered');
  await page.locator('#draft-compare').click();
  await expect(page.locator('#reconcile-dialog')).toBeVisible();
  const gap = await page.locator('#reconcile-apply').evaluate(element => {
    const a = element.getBoundingClientRect(),
      b = document.getElementById('reconcile-discard').getBoundingClientRect();
    return Math.max(b.top - a.bottom, b.left - a.right, a.left - b.right, a.top - b.bottom);
  });
  expect(gap, 'draft actions need a visible gap when they wrap').toBeGreaterThanOrEqual(8);
  await page.locator('#reconcile-close').click();
  await expect(page.locator('#reconcile-dialog')).toBeHidden();
});
