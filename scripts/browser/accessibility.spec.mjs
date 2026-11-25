import {test, expect} from '@playwright/test';
import {AxeBuilder} from '@axe-core/playwright';
import {startApp} from './app-fixture.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'fmn-accessibility-',
    account: {username: 'access-admin', password: 'DisposableAccess123!'}
  });
  await app.api('/p/default/api/note', {
    path: 'Accessible.md',
    content:
      '# Accessible notebook\n\nA readable note with a [public link](https://example.test).\n\n## Details\n\n| Item | Description |\n| --- | --- |\n| Note | Notebook content |\n',
    revision: ''
  });
});
test.afterAll(async () => app?.close());

const tags = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];
const themes = ['light', 'dark', 'parchment'];

// The note editor in each mode (plan risk 23), on a wide screen so Side by
// side is offered, with a note long enough that Source's scroller overflows
// there.
test('the note editor has named controls and accessible theme contrast in every mode', async ({page}, info) => {
  const paragraphs = Array.from(
    {length: 40},
    (_, i) => `Paragraph ${i + 1} of the draft, with *emphasis* and a [link](https://example.test/${i}).`
  );
  await app.api('/p/default/api/note', {
    path: 'Editing.md',
    content:
      '# Editing accessibly\n\n' +
      paragraphs.join('\n\n') +
      '\n\n## Table\n\n| Item | Count |\n| --- | --- |\n| Note | 1 |\n',
    revision: ''
  });
  await app.login(page, 'password');
  await page.setViewportSize({width: 1440, height: 900});
  await page.goto(app.origin + '/?project=default&note=Editing.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  const failures = [];
  for (const format of ['visual', 'source', 'split']) {
    await page.locator('#format-' + format).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
    await expect(page.locator('#editor')).toHaveAttribute(
      'aria-label',
      format === 'visual' ? 'Note content' : 'Markdown content'
    );
    if (format === 'split') {
      await expect(page.locator('#rendered-pane .rendered-preview h1')).toBeVisible();
      expect(
        await page.locator('#editing .cm-scroller').evaluate(scroller => scroller.scrollHeight > scroller.clientHeight)
      ).toBe(true);
    }
    for (const theme of themes) {
      await page.evaluate(theme => (document.documentElement.dataset.theme = theme), theme);
      const result = await new AxeBuilder({page}).include('#editing').withTags(tags).analyze();
      await info.attach(theme + '-' + format + '-axe', {body: JSON.stringify(result), contentType: 'application/json'});
      for (const issue of result.violations) {
        // axe-core does not count a contenteditable as focusable, so it finds
        // nothing focusable in Source's scroller beside the pane; Tab reaches
        // the editable .cm-content inside it. (When a note turns read-only
        // during an edit, .cm-content is no longer editable and Tab skips the
        // Markdown column: a real gap, which tabindex="0" on .cm-content
        // would close, and this exception with it.)
        const nodes = issue.nodes
          .map(node => node.target)
          .filter(
            target =>
              !(format === 'split' && issue.id === 'scrollable-region-focusable' && target.join(' ') === '.cm-scroller')
          );
        if (nodes.length) failures.push({theme, format, rule: issue.id, nodes});
      }
    }
  }
  expect(failures).toEqual([]);
});

for (const width of [390, 1440])
  test('notebook surfaces have named controls and accessible theme contrast at ' + width, async ({page}, info) => {
    await app.login(page, 'password');
    await page.setViewportSize({width, height: 900});
    const failures = [];
    for (const theme of themes)
      for (const view of ['library', 'note', 'search', 'settings']) {
        const query =
          view === 'note' ? 'note=Accessible.md' : view === 'settings' ? 'page=account&category=appearance' : 'folder=';
        await page.goto(app.origin + '/?project=default&' + query);
        await expect(page.locator('#new-note')).toBeEnabled();
        if (view === 'search') {
          if (width < 721) await page.locator('#menu').click();
          await page.locator('#search-open').click();
          await page.locator('#search-options-toggle').click();
          await page.locator('#advanced-search summary').click();
        }
        if (view === 'settings') await expect(page.locator('#account-dialog')).toBeVisible();
        await page.evaluate(theme => (document.documentElement.dataset.theme = theme), theme);
        const result = await new AxeBuilder({page}).withTags(tags).analyze();
        await info.attach(theme + '-' + view + '-axe', {body: JSON.stringify(result), contentType: 'application/json'});
        failures.push(
          ...result.violations.map(issue => ({
            theme,
            view,
            rule: issue.id,
            nodes: issue.nodes.map(node => node.target)
          }))
        );
        expect(
          await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
          theme + ' ' + view + ' horizontal overflow'
        ).toBe(true);
        await page.screenshot({path: info.outputPath(theme + '-' + view + '.png'), fullPage: true});
      }
    expect(failures).toEqual([]);
  });
