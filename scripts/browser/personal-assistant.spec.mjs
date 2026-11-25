import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({name: 'fmn-assistant-', account: {username: 'employee', password: 'DisposableAssistant123!'}});
  await app.api('/api/organization/action/set_assistant_policy', {
    enabled: true,
    aiConnection: 'luna',
    model: 'gpt-6-luna',
    reasoningEffort: 'low'
  });
});
test.afterAll(async () => {
  await app?.close();
});
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

// The assistant's profile files and the responsibilities records are notebook
// files: the browser keeps them as typed, as in the editor.
async function expectAsTyped(field) {
  for (const [name, value] of Object.entries({
    autocorrect: 'off',
    autocapitalize: 'off',
    writingsuggestions: 'false',
    spellcheck: 'true'
  }))
    await expect(field).toHaveAttribute(name, value);
}

test('employee can opt in, edit memory, exclude and restore a notebook', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#assistant-enabled')).toBeEnabled();
  await page.locator('#assistant-enabled').check();
  await page.locator('#assistant-consent').check();
  await page.locator('#assistant-features input[value=email]').check();
  await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
  await expect(page.locator('#assistant-message')).toHaveText('Assistant enabled.');
  await page.locator('summary').filter({hasText: 'Memory and preferences'}).click();
  await expect(page.getByRole('textbox', {name: 'SOUL.md', exact: true})).toBeVisible();
  await expectAsTyped(page.getByRole('textbox', {name: 'SOUL.md', exact: true}));
  await page.getByRole('textbox', {name: 'SOUL.md', exact: true}).fill('# Preferences\n\nKeep replies brief.');
  await page.getByRole('button', {name: 'Save SOUL.md', exact: true}).click();
  await expect(page.locator('#assistant-message')).toHaveText('Memory saved.');
  await page.locator('#assistant-exclusions').selectOption('default');
  await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
  await expect(page.locator('#assistant-exclusions option:checked')).toHaveAttribute('value', 'default');
  await page.locator('#assistant-exclusions').selectOption([]);
  await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
  await expect(page.locator('#assistant-exclusions option:checked')).toHaveCount(0);
  await page.reload();
  await expect(page.locator('#assistant-features input[value=email]')).toBeChecked();
  await page.locator('summary').filter({hasText: 'Memory and preferences'}).click();
  await expect(page.getByRole('textbox', {name: 'SOUL.md', exact: true})).toHaveValue(
    '# Preferences\n\nKeep replies brief.'
  );
});

test('assistant action failures show the server message instead of raw JSON', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#assistant-enabled')).toBeEnabled();
  await page.route('**/api/assistant/action/save_assistant_settings', route =>
    route.fulfill({
      status: 403,
      contentType: 'application/json',
      body: JSON.stringify({error: 'Assistant settings are locked by policy.', code: 'forbidden'})
    })
  );
  await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
  await expect(page.locator('#assistant-message')).toHaveText('Assistant settings are locked by policy.');
  await page.unroute('**/api/assistant/action/save_assistant_settings');
});

test('assistant preferences remain disabled until the saved context refresh settles', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#assistant-enabled')).toBeEnabled();
  await page.locator('#assistant-enabled').check();
  await page.locator('#assistant-consent').check();
  const save = page.getByRole('button', {name: 'Save preferences', exact: true});
  await save.click();
  await expect(save).toBeEnabled();
  await expect(page.locator('#assistant-exclusions option[value=default]')).toHaveCount(1);
  await page.locator('#assistant-exclusions').selectOption('default');
  let release,
    waiting = false;
  const gate = new Promise(resolve => (release = resolve));
  await page.route('**/api/assistant/action/assistant_context', async route => {
    waiting = true;
    await gate;
    await route.continue();
  });
  try {
    await save.click();
    await expect.poll(() => waiting).toBe(true);
    await expect(page.locator('#assistant-preferences > fieldset')).toHaveJSProperty('disabled', true);
    await expect(page.locator('#assistant-exclusions')).toBeDisabled();
    release();
    await expect(page.locator('#assistant-preferences > fieldset')).toHaveJSProperty('disabled', false);
    await expect(page.locator('#assistant-message')).toHaveText('Assistant enabled.');
    await page.locator('#assistant-exclusions').selectOption([]);
    await save.click();
    await expect(save).toBeEnabled();
    await expect(page.locator('#assistant-exclusions option:checked')).toHaveCount(0);
    await page.reload();
    await expect(page.locator('#assistant-exclusions option[value=default]')).toHaveCount(1);
    await expect(page.locator('#assistant-exclusions option:checked')).toHaveCount(0);
  } finally {
    release();
  }
});

test('reply reminders explain that they need task queues', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#reply-setup')).toBeVisible();
  await expect(page.locator('#assistant-replies')).not.toBeChecked();
  await page.locator('#assistant-replies').check();
  await page.getByRole('button', {name: 'Save reply reminders', exact: true}).click();
  await expect(page.locator('#assistant-message')).toContainText('needs task queues');
  await page.reload();
  await expect(page.locator('#assistant-replies')).not.toBeChecked();
});

test('memory consent works while personal assistants are off', async ({page}) => {
  const plain = await startApp({name: 'fmn-memory-', account: {username: 'writer', password: 'DisposableMemory123!'}});
  try {
    await plain.login(page, 'password');
    await page.goto(plain.origin + '/account/assistant');
    await expect(page.locator('#assistant-message')).toContainText('Personal assistants are disabled');
    await expect(page.locator('#assistant-preferences')).toBeHidden();
    await expect(page.locator('#reply-setup')).toBeHidden();
    await expect(page.locator('#memory-notebook')).toBeHidden();
    await expect(page.locator('#assistant-memory')).toBeChecked();
    await page.locator('#assistant-memory').uncheck();
    await page.getByRole('button', {name: 'Save memory setting', exact: true}).click();
    await expect(page.locator('#assistant-message')).toHaveText('Memory setting saved.');
    await expect(page.locator('#memory-notebook')).toBeVisible();
    await page.reload();
    await expect(page.locator('#assistant-memory')).not.toBeChecked();
    await expect(page.locator('#assistant-enabled')).not.toBeChecked();
  } finally {
    await plain.close();
  }
});

test('changing the assistant policy updates the preferences without a reload', async ({page}) => {
  const plain = await startApp({
    name: 'fmn-assistant-policy-',
    account: {username: 'operator', password: 'DisposablePolicy123!'}
  });
  try {
    await plain.login(page, 'password');
    await page.goto(plain.origin + '/account/assistant');
    await expect(page.locator('#assistant-message')).toContainText('Personal assistants are disabled');
    await expect(page.locator('#assistant-preferences')).toBeHidden();
    await page.locator('#assistant-admin > summary').click();
    await expectAsTyped(page.getByRole('textbox', {name: 'Responsibilities and source links', exact: true}));
    await page.locator('#assistant-policy-enabled').check();
    await page.locator('#assistant-connection').fill('luna');
    await page.locator('#assistant-model').fill('gpt-6-luna');
    const savePolicy = page.getByRole('button', {name: 'Save assistant policy', exact: true});
    await savePolicy.click();
    await expect(page.locator('#assistant-preferences')).toBeVisible();
    await expect(savePolicy).toBeEnabled();
    await expect(page.locator('#assistant-preferences > fieldset')).toHaveJSProperty('disabled', false);
    await expect(page.locator('#assistant-enabled')).toBeEnabled();
    await page.locator('#assistant-policy-enabled').uncheck();
    await savePolicy.click();
    await expect(page.locator('#assistant-preferences')).toBeHidden();
    await expect(savePolicy).toBeEnabled();
    await expect(page.locator('#assistant-preferences > fieldset')).toHaveJSProperty('disabled', true);
  } finally {
    await plain.close();
  }
});

for (const [width, theme] of [
  [1280, 'light'],
  [390, 'dark']
])
  test(`assistant layout ${width} ${theme}`, async ({page}, info) => {
    await page.setViewportSize({width, height: 900});
    await page.addInitScript(theme => localStorage.setItem('gitnotes:theme', theme), theme);
    await app.login(page, 'password');
    await page.goto(app.origin + '/account/assistant');
    await expect(page.locator('#assistant-features input')).toHaveCount(8);
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
    const errors = await page.locator('main').evaluate(root => {
      const issues = [];
      if (document.documentElement.scrollWidth > innerWidth) issues.push('horizontal overflow');
      for (const input of root.querySelectorAll('input[type=checkbox]')) {
        if (!input.checkVisibility()) continue;
        const box = input.getBoundingClientRect(),
          label = input.closest('label').getBoundingClientRect();
        if (box.width < 14 || box.width > 24 || box.top > label.top + 12) issues.push('misaligned ' + input.id);
      }
      return issues;
    });
    expect(errors).toEqual([]);
    await page.screenshot({path: info.outputPath('assistant.png'), fullPage: true});
  });
