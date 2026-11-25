import {test, expect} from '@playwright/test';
import {createServer} from 'node:http';
import {createHash} from 'node:crypto';
import {startApp} from './app-fixture.mjs';

test('OAuth consent follows a client callback redirect to another origin', async ({page}) => {
  const app = await startApp({
    name: 'forgetmenote-oauth-',
    account: {username: 'oauth-admin', password: 'DisposableOAuth123!'}
  });
  const callback = createServer((request, response) => {
    const url = new URL(request.url, 'http://localhost');
    if (url.pathname === '/callback') {
      response.writeHead(302, {Location: `http://127.0.0.1:${callback.address().port}/connected${url.search}`});
      response.end();
    } else {
      response.writeHead(200, {'Content-Type': 'text/plain'});
      response.end('Client connected');
    }
  });
  try {
    await new Promise(resolve => callback.listen(0, '127.0.0.1', resolve));
    const redirect = `http://localhost:${callback.address().port}/callback`;
    const registration = await page.request.post(app.origin + '/oauth/register', {
      data: {client_name: 'Redirecting client', redirect_uris: [redirect]}
    });
    expect(registration.status()).toBe(201);
    const client = await registration.json();
    const verifier = 'v'.repeat(43);
    const query = new URLSearchParams({
      client_id: client.client_id,
      redirect_uri: redirect,
      response_type: 'code',
      code_challenge_method: 'S256',
      code_challenge: createHash('sha256').update(verifier).digest('base64url'),
      state: 'callback-state'
    });
    await app.login(page, 'password');
    await page.goto(app.origin + '/oauth/authorize?' + query);
    await page.getByRole('button', {name: 'Allow access'}).click();
    await expect(page).toHaveURL(new RegExp(`^http://127\\.0\\.0\\.1:${callback.address().port}/connected\\?`));
    await expect(page.getByText('Client connected', {exact: true})).toBeVisible();
    const result = new URL(page.url()).searchParams;
    expect(result.get('state')).toBe('callback-state');
    expect(result.get('iss')).toBe(app.origin);
    const token = await page.request.post(app.origin + '/oauth/token', {
      form: {
        grant_type: 'authorization_code',
        client_id: client.client_id,
        redirect_uri: redirect,
        code_verifier: verifier,
        code: result.get('code')
      }
    });
    expect(token.status()).toBe(200);
    expect((await token.json()).access_token).toBeTruthy();
    const login = await page.request.get(app.origin + '/login');
    expect(login.headers()['content-security-policy']).toContain("form-action 'self'");
  } finally {
    await new Promise(resolve => callback.close(resolve));
    await app.close();
  }
});
