import {test, expect} from '@playwright/test';
import {readFile} from 'node:fs/promises';
const script = (await readFile(new URL('../../internal/chat/website_load.js', import.meta.url), 'utf8')).replace(
  '__FMN_CAPTURE_TOKEN__',
  'synthetic-loading'
);
const png = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nGAAAAAASUVORK5CYII=',
  'base64'
);
async function report(page, dismiss = false) {
  await page.addScriptTag({content: script.replace('__FMN_ALLOW_PROMOTION_DISMISSAL__', String(dismiss))});
  await page.locator('meta[name="synthetic-loading"]').waitFor({state: 'attached', timeout: 12000});
  return JSON.parse(await page.locator('meta[name="synthetic-loading"]').getAttribute('content'));
}
test('capture scroll activates lazy images and entrance content then returns to top', async ({page}) => {
  await page.route('https://fixture.example/image.png', route => route.fulfill({contentType: 'image/png', body: png}));
  await page.setContent(
    `<div style="height:2400px">Top</div><section id="content" style="opacity:0"><h2>Why choose us</h2><img id="photo" width="128" height="128" loading="lazy"></section><footer>Bottom</footer><script>new IntersectionObserver(entries=>{if(entries.some(e=>e.isIntersecting)){document.querySelector('#content').style.opacity='1';document.querySelector('#photo').src='https://fixture.example/image.png';}}).observe(document.querySelector('#content'));</script>`
  );
  const load = await report(page);
  expect(load.reachedBottom).toBe(true);
  expect(load.timedOut).toBe(false);
  expect(load.pendingImages).toBe(0);
  expect(load.brokenImages).toBe(0);
  expect(await page.evaluate(() => scrollY)).toBe(0);
  expect(await page.locator('#content').evaluate(el => getComputedStyle(el).opacity)).toBe('1');
  expect(await page.locator('#photo').evaluate(img => img.complete && img.naturalWidth > 0)).toBe(true);
});
test('capture reports a broken visible image without hiding original content', async ({page}) => {
  await page.route('https://fixture.example/broken.png', route => route.fulfill({status: 404, body: 'missing'}));
  await page.setContent(
    '<h1>Original content</h1><img src="https://fixture.example/broken.png" width="300" height="300">'
  );
  const load = await report(page);
  expect(load.reachedBottom).toBe(true);
  expect(load.brokenImages).toBe(1);
  await expect(page.locator('h1')).toHaveText('Original content');
});
test('capture bounds scrolling on an endlessly growing page', async ({page}) => {
  await page.setContent(
    '<div style="height:1600px">Top</div><script>addEventListener("scroll",()=>{if(scrollY+innerHeight>document.documentElement.scrollHeight-300){const more=document.createElement("div");more.style.height="1600px";document.body.append(more);}});</script>'
  );
  const load = await report(page);
  expect(load.reachedBottom).toBe(false);
  expect(load.timedOut).toBe(true);
  expect(await page.evaluate(() => scrollY)).toBe(0);
});

test('capture detects page growth while images settle after reaching the footer', async ({page}) => {
  await page.setContent(
    '<img width="100" height="100" src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nGAAAAAASUVORK5CYII="><footer>Bottom</footer><script>HTMLImageElement.prototype.decode=async function(){await new Promise(r=>setTimeout(r,200));const more=document.createElement("div");more.style.height="2000px";document.body.append(more);};</script>'
  );
  const load = await report(page);
  expect(load.reachedBottom).toBe(false);
  expect(load.timedOut).toBe(false);
  expect(load.documentHeight).toBeGreaterThan(2000);
});

test('capture closes an ordinary promotional overlay but preserves an age gate', async ({page}) => {
  await page.setContent(
    '<h1>Public business page</h1><div class="sqs-popup-overlay"><p>Subscribe to our newsletter for special offers</p><button class="sqs-popup-overlay-close" onclick="setTimeout(() => this.parentElement.remove(), 50)">Close</button></div><div role="dialog" id="age"><p>Subscribe after verifying your age. Adults only 18+</p><button aria-label="Close popup" onclick="this.parentElement.remove()">Close</button></div>'
  );
  const load = await report(page, true);
  expect(load.dismissedOverlays).toBe(1);
  await expect(page.locator('.sqs-popup-overlay')).toHaveCount(0);
  await expect(page.locator('#age')).toBeVisible();
});

test('initial capture preserves public overlays and retry cannot open an age consent gate', async ({page}) => {
  await page.setContent(
    '<div class="sqs-popup-overlay" id="promotion">Newsletter special offer<button class="sqs-popup-overlay-close" onclick="this.parentElement.remove()">Close</button></div>'
  );
  expect((await report(page)).dismissedOverlays).toBe(0);
  await expect(page.locator('#promotion')).toBeVisible();
  await page.setContent(
    '<div role="dialog" id="gate">Confirm you are over 18 years old to continue. We use cookies.<button aria-label="Close popup" onclick="this.parentElement.remove()">Close</button></div>'
  );
  expect((await report(page, true)).dismissedOverlays).toBe(0);
  await expect(page.locator('#gate')).toBeVisible();
});

test('promotional enter text is closable while age and access gates remain', async ({page}) => {
  await page.setContent(
    '<div class="sqs-popup-overlay">Proud to be named best business. Enter site<button class="sqs-popup-overlay-close" onclick="this.parentElement.remove()">Close</button></div><div role="dialog" id="gate">Members only. Continue to sign in<button aria-label="Close dialog" onclick="this.parentElement.remove()">Close</button></div><div class="sqs-cookie-banner">Cookies<button onclick="this.parentElement.remove()">Decline</button></div>'
  );
  const load = await report(page, true);
  expect(load.dismissedOverlays).toBe(2);
  await expect(page.locator('#gate')).toBeVisible();
});

test('capture observes lazy frames without asserting their media is rendered', async ({page}) => {
  await page.route('https://fixture.example/frame', route =>
    route.fulfill({contentType: 'text/html', body: '<p>Loaded frame</p>'})
  );
  await page.setContent(
    '<div style="height:1500px"></div><iframe width="300" height="200" loading="lazy" src="https://fixture.example/frame"></iframe><video width="200" height="100"></video>'
  );
  const load = await report(page);
  expect(load.version).toBe(3);
  expect(load.frameCount).toBe(1);
  expect(load.pendingFrames).toBe(0);
  expect(load.videoCount).toBe(1);
  expect(load.pendingVideos).toBe(1);
  expect(await page.locator('iframe').getAttribute('loading')).toBe('eager');
});

for (const wording of [
  'You must be 18 or older. Continue.',
  'Are you 18?',
  'You must be 18 to continue',
  'I am at least 21'
]) {
  test(`capture preserves structural age prompt: ${wording}`, async ({page}) => {
    await page.setContent(
      `<div role="dialog" id="gate">${wording}<button aria-label="Close dialog" onclick="this.parentElement.remove()">Close</button></div>`
    );
    const load = await report(page, true);
    expect(load.dismissedOverlays).toBe(0);
    await expect(page.locator('#gate')).toBeVisible();
  });
}

test('already loaded cross-origin frames remain explicitly unobserved', async ({page}) => {
  await page.route('https://fixture.example/prior-frame', route =>
    route.fulfill({contentType: 'text/html', body: '<p>Frame ready</p>'})
  );
  await page.setContent('<iframe width="300" height="200" src="https://fixture.example/prior-frame"></iframe>');
  await page.frameLocator('iframe').locator('p').waitFor();
  const load = await report(page);
  expect(load.frameCount).toBe(1);
  expect(load.pendingFrames).toBe(0);
  expect(load.unobservedFrames).toBe(1);
});

test('capture reports unloaded video embed placeholders separately from broken photos', async ({page}) => {
  await page.route('https://fixture.example/broken.png', route => route.fulfill({status: 404, body: 'missing'}));
  await page.setContent(
    '<h1>Inspectable business page</h1><div data-widget_type="video.default" style="width:300px;height:180px;background:black"></div><img src="https://fixture.example/broken.png" width="200" height="100">'
  );
  const load = await report(page);
  expect(load.embedPlaceholderCount).toBe(1);
  expect(load.brokenImages).toBe(1);
  expect(load.frameCount).toBe(0);
});
