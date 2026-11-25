// Screenshot the main signed-in surfaces of a disposable instance for visual review.
// Usage from the repository root: `node scripts/ui-capture.mjs OUT_DIR`.
// The browser fixture builds this checkout, creates a synthetic account and never
// exposes the instance's recovery key; all data is deleted afterwards.
import {chromium} from '@playwright/test';
import {mkdir} from 'node:fs/promises';
import {join, resolve} from 'node:path';
import {startApp} from './browser/app-fixture.mjs';

const out = resolve(process.argv[2] || 'ui-capture');
await mkdir(out, {recursive: true});
const account = {username: 'capture-admin', password: 'DisposableCapture2026!x'};
const app = await startApp({name: 'forgetmenote-ui-capture-', account});
const protocol = `---
tags: [method, water]
status: Active
---
# Sampling protocol

Every sample we take should be repeatable by someone who has never visited the site.

## Before you leave

- Check the tide table and plan to arrive **two hours before low water**.
- Pack the cooler, sterile bottles and the field tablet.

## At the site

1. Rinse each bottle three times with site water.
2. Record temperature, salinity and turbidity in the log.

> If turbidity is above 40 NTU, take a duplicate sample.

| Site | Depth (m) | Bottles | Notes |
|---|---|---|---|
| Mill Creek | 0.5 | 3 | Access from the boat ramp |
| North Flats | 1.2 | 2 | Soft mud, wear waders |

Inline values like \`site_id\` must match the register.
`;
const notes = {
  'Field guide/Sampling protocol.md': protocol,
  'Field guide/Equipment checklist.md': '---\ntags: [method]\n---\n# Equipment checklist\n\nWhat goes in the van before every field day.',
  'Sites/Mill Creek.md': '---\ntags: [sites]\nstatus: Active\n---\n# Mill Creek\n\nTidal creek below the old mill.',
  'Weekly notes/2026-W40.md': '# Week 40\n\nDuplicate samples taken at Mill Creek after the storm.',
  'Reading list.md': '---\ntags: [reading]\n---\n# Reading list\n\nPapers and field manuals worth reading.',
  'Open questions.md': '---\ntags: [planning]\nstatus: Draft\n---\n# Open questions\n\nWinter sampling cadence and a second probe.',
};
await app.api('/api/projects', {id: 'estuary', name: 'Estuary Monitoring'});
for (const [path, content] of Object.entries(notes)) await app.api('/p/estuary/api/note', {path, content, revision: ''});

const browser = await chromium.launch();
const base = app.origin + '/?project=estuary';
const protocolURL = base + '&note=' + encodeURIComponent('Field guide/Sampling protocol.md');
async function session(viewport, theme) {
  const context = await browser.newContext({viewport, deviceScaleFactor: 2});
  const page = await context.newPage();
  await app.login(page, 'password');
  if (theme) await page.addInitScript(value => addEventListener('DOMContentLoaded', () => {document.documentElement.dataset.theme = value;}), theme);
  return {context, page};
}
// A note with live changes (the focus edit below) keeps a long poll open, so the network never idles.
async function open(page, url) { await page.goto(url); await page.waitForLoadState('networkidle', {timeout: 5000}).catch(() => {}); await page.waitForTimeout(600); }
// A reload resumes a live editing session, so Edit may already be Read.
async function edit(page) { if (await page.locator('#editing').isHidden()) await page.locator('#mode').click(); await page.locator('#editor[aria-label="Note content"]').waitFor(); }
async function read(page) { if (await page.locator('#editing').isVisible()) await page.locator('#mode').click(); await page.locator('#preview').waitFor(); }
async function shot(page, name) { await page.screenshot({path: join(out, name + '.png')}); console.log(join(out, name + '.png')); }
// Desktop focus mode keeps its toolbar below the bottom edge until the pointer reaches it.
async function focusToolbar(page, shown) {
  const {width, height} = page.viewportSize();
  await page.mouse.move(width / 2, shown ? height - 2 : height / 3);
  await page.waitForFunction(shown => document.querySelector('#main > header').hasAttribute('data-revealed') === shown, shown);
  await page.waitForTimeout(300);
}

try {
  const {context, page} = await session({width: 1440, height: 900});
  await open(page, base + '&folder='); await shot(page, 'library');
  await page.locator('#account-actions-toggle').click(); await shot(page, 'account-menu'); await page.keyboard.press('Escape');
  await page.locator('#search-open').click(); await page.locator('#search').fill('sample');
  await page.locator('#search-status').filter({hasText: /result/}).waitFor(); await shot(page, 'search');
  await page.locator('#search-options-toggle').click(); await shot(page, 'search-filters');
  await open(page, protocolURL); await shot(page, 'read');
  await page.locator('#note-details-toggle').click(); await shot(page, 'read-more-menu'); await page.keyboard.press('Escape');
  await page.locator('#note-context-toggle').click(); await shot(page, 'read-context'); await page.keyboard.press('Escape');
  await edit(page); await shot(page, 'edit-visual');
  await page.locator('#format-source').click(); await page.locator('#editor').waitFor(); await shot(page, 'edit-source');
  await open(page, protocolURL); await page.locator('#focus-toggle').click(); await focusToolbar(page, false); await shot(page, 'focus-read');
  await focusToolbar(page, true); await shot(page, 'focus-toolbar');
  await edit(page);
  await page.locator('#editor .cm-line').filter({hasText: /\w/}).nth(1).click(); await page.keyboard.press('End'); await page.keyboard.type(' Unsaved focus edit.');
  await focusToolbar(page, false); await shot(page, 'focus-edit');
  await context.close();

  const dark = await session({width: 1440, height: 900}, 'dark');
  await open(dark.page, protocolURL); await shot(dark.page, 'read-dark');
  await edit(dark.page);
  await dark.page.locator('#focus-toggle').click(); await focusToolbar(dark.page, true); await dark.page.locator('#focus-dim').click();
  await dark.page.locator('#editor .cm-line').filter({hasText: /\w/}).nth(1).click(); await focusToolbar(dark.page, true); await shot(dark.page, 'focus-edit-dark');
  await dark.page.keyboard.press('Escape');
  await open(dark.page, base + '&folder='); await shot(dark.page, 'library-dark');
  await dark.context.close();

  const phone = await session({width: 390, height: 844});
  await open(phone.page, protocolURL); await shot(phone.page, 'phone-read');
  await phone.page.locator('#menu').click(); await shot(phone.page, 'phone-sidebar');
  await open(phone.page, protocolURL); await phone.page.locator('#focus-toggle').click(); await read(phone.page); await shot(phone.page, 'phone-focus-read');
  await edit(phone.page); await shot(phone.page, 'phone-focus-edit');
  await phone.page.locator('#focus-toggle').click();
  await open(phone.page, base + '&folder=');
  await phone.page.locator('[data-destination="search-open"]').click(); await phone.page.locator('#search').fill('sample');
  await phone.page.locator('#search-status').filter({hasText: /result/}).waitFor(); await shot(phone.page, 'phone-search');
  await phone.context.close();
} finally {
  await browser.close();
  await app.close();
}
