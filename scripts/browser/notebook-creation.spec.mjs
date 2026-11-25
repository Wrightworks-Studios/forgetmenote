import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-creation-',
    account: {username: 'creation-admin', password: 'DisposableCreation123!'}
  });
});
test.afterAll(async () => app?.close());

for (const width of [1280, 390]) {
  test(`notebook creation choices and consent align at ${width}px`, async ({page}, testInfo) => {
    await page.setViewportSize({width, height: 900});
    await page.emulateMedia({colorScheme: 'light'});
    await app.login(page, 'password');
    await page.route('**/api/projects/repository/choices?*', route =>
      route.fulfill({
        json: {
          project: 'example-source',
          name: 'Example source',
          choices: [{id: 123, installation: 456, full_name: 'example/application-source', private: true}],
          installUrl: 'https://github.com/apps/example/installations/new'
        }
      })
    );
    await page.goto(app.origin + '/?repository=create&notebook=example-source');
    const dialog = page.locator('#workspace-create-dialog');
    await expect(page.locator('#notebook-repository-selection')).toBeVisible();
    for (const id of ['notebook-repository-share']) {
      const checkbox = page.locator('#' + id);
      const box = await checkbox.boundingBox();
      const label = await checkbox.locator('..').boundingBox();
      expect(box.width, 'checkbox must not inherit full-width text input styling').toBeLessThanOrEqual(24);
      expect(Math.abs(box.x - label.x), 'checkbox belongs at the start of its label').toBeLessThanOrEqual(2);
      await checkbox.locator('..').click();
      await expect(checkbox).toBeChecked();
    }
    await expect(page.locator('#notebook-repository-external')).toHaveCount(0);
    const modes = await dialog.locator('[aria-label="New notebook options"] button').all();
    const boxes = await Promise.all(modes.map(mode => mode.boundingBox()));
    for (const box of boxes) {
      expect(Math.abs(box.width - boxes[0].width), 'mode buttons must have equal widths').toBeLessThanOrEqual(1);
      expect(Math.abs(box.height - boxes[0].height), 'mode buttons must have equal heights').toBeLessThanOrEqual(1);
      if (width > 640) expect(Math.abs(box.y - boxes[0].y)).toBeLessThanOrEqual(1);
      else expect(Math.abs(box.x - boxes[0].x)).toBeLessThanOrEqual(1);
    }
    expect(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
    await dialog.screenshot({path: testInfo.outputPath('repository.png')});
    await page.locator('#notebook-import-mode').click();
    await expect(page.locator('#notebook-bundle')).toBeVisible();
    await expect(page.locator('#notebook-bundle')).toBeEnabled();
    await expect(page.locator('#notebook-repository-fields')).toBeHidden();
    await expect(page.locator('#notebook-submit')).toHaveText('Import notebook');
    expect(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
    await dialog.screenshot({path: testInfo.outputPath('import.png')});
    await page.locator('#notebook-create-mode').click();
    await expect(page.locator('#notebook-bundle')).toBeDisabled();
    await expect(page.locator('#notebook-submit')).toHaveText('Create notebook');
  });
}
