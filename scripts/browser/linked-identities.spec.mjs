import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app, linked, other;
const account = {username: 'identities-admin', password: 'DisposableIdentities123!'};
const linkedAccount = {username: 'identity-person', password: 'DisposablePerson123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'fmn-linked-identities-', account});
  linked = await app.api('/api/local-users', linkedAccount);
  other = await app.api('/api/local-users', {username: 'identity-other', password: 'DisposableOther123!'});
});
test.afterAll(async () => app?.close());
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

async function person(page, username) {
  await page.locator('#access-list').getByRole('button', {name: username}).click();
  await expect(page.locator('#user-heading')).toHaveText(username);
}

test('an Admin links a sign-in identity to a person, and the server refuses linking it twice', async ({
  page,
  browser
}) => {
  await app.login(page, 'password');
  await page.goto(app.origin);
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await person(page, linkedAccount.username);
  const bind = page.locator('#external-identity-bind');
  await bind.getByText('Link a sign-in identity', {exact: true}).click();
  await expect(bind).toContainText('never an email address');
  await bind.getByLabel('Issuer', {exact: true}).fill('https://id.example.com');
  await bind.getByLabel('Subject', {exact: true}).fill('synthetic-subject-1');
  await bind.getByRole('button', {name: 'Link identity', exact: true}).click();
  await expect(page.locator('#admin-status')).toHaveText('Sign-in identity linked.');
  await expect(bind.getByLabel('Subject', {exact: true})).toHaveValue('');

  // The person's own account page lists the identity the Admin linked.
  const context = await browser.newContext();
  try {
    const login = await context.request.post(app.origin + '/auth/password/login', {
      form: linkedAccount,
      headers: {Origin: app.origin}
    });
    expect(login.ok()).toBe(true);
    const own = await (await context.request.get(app.origin + '/account')).json();
    expect(own.oidc.map(({issuer, subject, user}) => ({issuer, subject, user}))).toEqual([
      {issuer: 'https://id.example.com', subject: 'synthetic-subject-1', user: linked.id}
    ]);
  } finally {
    await context.close();
  }

  // The same identity cannot be linked to someone else; the server says why.
  await person(page, 'identity-other');
  await bind.getByText('Link a sign-in identity', {exact: true}).click();
  await bind.getByLabel('Issuer', {exact: true}).fill('https://id.example.com');
  await bind.getByLabel('Subject', {exact: true}).fill('synthetic-subject-1');
  await bind.getByRole('button', {name: 'Link identity', exact: true}).click();
  await expect(page.locator('#admin-status')).toHaveText('external identity already bound');

  await page.locator('#admin-dialog [data-settings-category=activity]').click();
  await expect(page.locator('#activity-list')).toContainText('account.external.bind');

  // A disabled person signs in with nothing, so the form is not offered.
  await app.api('/api/users', {id: other.id, disabled: true});
  await page.locator('#admin-dialog [data-settings-category=people]').click();
  await page.locator('[data-close=admin-dialog]').click();
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await person(page, 'identity-other');
  await expect(bind).toBeHidden();
});
