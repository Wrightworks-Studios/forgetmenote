import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'fmn-settings-layout-',
    account: {username: 'layout-admin', password: 'DisposableLayout123!'}
  });
});
test.afterAll(async () => {
  await app?.close();
});
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

async function geometry(page, dialog) {
  const problems = await dialog.evaluate(root => {
    const errors = [];
    if (root.scrollWidth > root.clientWidth + 2) errors.push('dialog overflows horizontally');
    for (const input of root.querySelectorAll('input[type=checkbox],input[type=radio]')) {
      if (!input.checkVisibility({checkOpacity: true, checkVisibilityCSS: true})) continue;
      const label = input.closest('label');
      if (!label) {
        errors.push((input.id || input.name) + ': detached check label');
        continue;
      }
      const box = input.getBoundingClientRect(),
        row = label.getBoundingClientRect();
      if (box.width > 24 || box.height > 24 || box.width < 14)
        errors.push((input.id || label.textContent) + ': wrong check size');
      if (box.x < row.x - 2 || box.right > row.right + 2 || box.y > row.y + 12)
        errors.push((input.id || label.textContent) + ': check not aligned with label');
      const next = label.nextElementSibling;
      if (next?.tagName === 'BUTTON' && next.getBoundingClientRect().top < box.bottom)
        errors.push(input.id + ': button overlaps check');
    }
    return errors;
  });
  expect(problems).toEqual([]);
}

for (const width of [1280, 390])
  for (const theme of ['light', 'dark', 'parchment'])
    test(`settings forms and global AI controls at ${width}px in ${theme}`, async ({page}, info) => {
      await page.setViewportSize({width, height: 900});
      await page.emulateMedia({colorScheme: theme === 'dark' ? 'dark' : 'light'});
      await app.login(page, 'password');
      await page.route('**/api/organization/action/ai_settings', async route => {
        const response = await route.fetch();
        const data = await response.json();
        data.providers = {embeddings: true, decisions: true, localOnly: false};
        await route.fulfill({json: data});
      });
      await page.addInitScript(theme => localStorage.setItem('gitnotes:theme', theme), theme);
      await page.goto(app.origin);
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
      if (width < 720) await page.locator('#menu').click();
      await page.locator('#account-actions-toggle').click();
      await page.locator('#admin-open').click();
      const admin = page.locator('#admin-dialog');
      await expect(admin).toBeVisible();
      await admin.getByRole('button', {name: 'AI features', exact: true}).click();
      await expect(page.locator('#instance-ai-form')).toBeVisible();
      await expect(page.locator('#instance-ai-embeddings')).toBeEnabled();
      await page.locator('#instance-ai-embeddings').check();
      await expect(page.locator('#instance-ai-model')).toBeVisible();
      await geometry(page, admin);
      await page.screenshot({path: info.outputPath(`administration-ai-${width}-${theme}.png`)});
      for (const button of await admin.locator('[data-settings-categories] button').all()) {
        await button.click();
        await geometry(page, admin);
      }
      await admin.locator('[data-settings-category=people]').click();
      await page.locator('#access-list button').first().click();
      await geometry(page, admin);
      await admin.getByRole('button', {name: 'Close', exact: true}).click();
      await expect(admin).toBeHidden();
      if (width < 720) await page.locator('#menu').click();
      await page.locator('#settings-open').click();
      const notebook = page.locator('#settings-dialog');
      await expect(notebook.getByRole('button', {name: 'AI workflows', exact: true})).toHaveCount(0);
      for (const button of await notebook.locator('[data-settings-categories] button:visible').all()) {
        await button.click();
        await geometry(page, notebook);
      }
      await notebook.locator('[data-settings-category=search]').click();
      await page.locator('#semantic-settings-open').click();
      await expect(page.locator('#knowledge-dialog')).toBeVisible();
      await expect(page.locator('#semantic-enabled')).toHaveCount(0);
      await expect(page.locator('#knowledge-dialog')).toContainText('Administration');
      await geometry(page, page.locator('#knowledge-dialog'));
    });

test('automation account identifiers wrap on narrow screens', async ({page}) => {
  const directory = await app.api('/api/admin');
  const admin = directory.users.find(user => user.admin && user.id !== '@local-admin');
  const service = await app.api('/api/organization/action/create_service_account', {
    name: 'Layout worker',
    steward: admin.id
  });
  // A retained identifier makes the geometry independent of random glyph widths.
  const identifier = 'service-gg3tl5go4cbxkhs5uk5n6vqaa3f66swkum6a4o56fxajhdk45sez';
  await page.route('**/api/organization/action/offboarding_status', async route => {
    const response = await route.fetch();
    const data = await response.json();
    if (data.account.user.id === service.id) data.account.user.id = identifier;
    await route.fulfill({json: data});
  });
  await app.login(page, 'password');
  await page.goto(app.origin);
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await page.locator('#admin-dialog [data-settings-category=automation]').click();
  await page.locator('#service-list').getByRole('button', {name: 'Layout worker', exact: true}).click();
  await expect(page.locator('#service-details')).toContainText(identifier);
  await page.evaluate(() => document.fonts.ready);
  for (const width of [390, 320]) {
    await page.setViewportSize({width, height: 844});
    const layout = await page.locator('#admin-dialog').evaluate(dialog => {
      const element = dialog.querySelector('#service-details > p'),
        range = document.createRange();
      range.selectNodeContents(element);
      return {
        width: dialog.clientWidth,
        scroll: dialog.scrollWidth,
        textRight: Math.max(...[...range.getClientRects()].map(rect => rect.right)),
        right: element.getBoundingClientRect().right
      };
    });
    expect(layout.scroll, JSON.stringify(layout)).toBeLessThanOrEqual(layout.width + 1);
    expect(layout.textRight, JSON.stringify(layout)).toBeLessThanOrEqual(layout.right + 1);
  }
});

for (const width of [1280, 390])
  test(`settings pages keep their heading and categories in place across tabs, and AI hints, at ${width}px`, async ({
    context
  }) => {
    const signIn = await context.newPage();
    await app.login(signIn, 'password');
    await signIn.close();
    for (const name of ['notebook', 'administration', 'account']) {
      // Each settings page opens fresh, as from a bookmark.
      const page = await context.newPage();
      await page.setViewportSize({width, height: 900});
      await page.goto(app.origin + '/?project=default&page=' + name);
      const dialog = page.locator('#settings-page .settings-page-panel[open]');
      await expect(dialog).toBeVisible();
      const heading = dialog.locator('.dialog-title h2');
      await expect(heading).toBeFocused();
      // The heading is a programmatic focus target, not a control: no focus ring.
      expect(await heading.evaluate(h => getComputedStyle(h).outlineStyle)).toBe('none');
      // The page has one way back: the header close control.
      await expect(page.locator('.settings-breadcrumb')).toHaveCount(0);
      await expect(dialog.getByRole('button', {name: 'Sign out', exact: true})).toHaveCount(0);
      const positions = [];
      for (const button of await dialog.locator('[data-settings-categories] button:visible').all()) {
        await button.click();
        positions.push(
          await dialog.evaluate(root => {
            const panel = [...root.querySelectorAll('[data-settings-panel]')].find(p => p.checkVisibility());
            return {
              heading: Math.round(root.querySelector('.dialog-title h2').getBoundingClientRect().top + scrollY),
              nav: Math.round(root.querySelector('[data-settings-categories]').getBoundingClientRect().top + scrollY),
              panel: Math.round(panel.getBoundingClientRect().top + scrollY),
              navWraps: (() => {
                const tops = [...root.querySelectorAll('[data-settings-categories] button')]
                  .filter(b => b.checkVisibility())
                  .map(b => Math.round(b.getBoundingClientRect().top));
                return (
                  getComputedStyle(root.querySelector('[data-settings-categories]')).flexDirection === 'row' &&
                  new Set(tops).size > 1
                );
              })()
            };
          })
        );
      }
      expect(positions.length, name).toBeGreaterThan(1);
      for (const position of positions) {
        expect({...position, navWraps: false}, name).toEqual({...positions[0], navWraps: false});
        // On phones the categories are one horizontally scrolling row.
        expect(position.navWraps, name).toBe(false);
      }
      // Categories start close below the header divider instead of after an empty band.
      const title = await dialog.locator('.dialog-title').boundingBox();
      const nav = await dialog.locator('[data-settings-categories]').boundingBox();
      const scope = (await dialog.locator('.settings-scope').count())
        ? await dialog.locator('.settings-scope').boundingBox()
        : null;
      expect(nav.y - (scope ? scope.y + scope.height : title.y + title.height), name).toBeLessThanOrEqual(36);
      await page.close();
    }
    // Unavailable AI features say which provider they need.
    const page = await context.newPage();
    await page.goto(app.origin + '/?project=default&page=administration&category=ai');
    await expect(page.locator('#instance-ai-form')).toBeVisible();
    const embeddings = page.locator('#instance-ai-embeddings');
    await expect(embeddings).toBeDisabled();
    await expect(embeddings).toHaveAccessibleDescription('Requires a configured embeddings provider.');
    await expect(page.locator('#instance-ai-writing')).toBeEnabled();
    await expect(page.locator('#instance-ai-writing')).toHaveAccessibleDescription('');
    const uploads = page.locator('#instance-ai-uploadReading');
    await expect(uploads).toBeDisabled();
    await expect(uploads).toHaveAccessibleDescription('Requires a chat connection that reads images.');
  });
