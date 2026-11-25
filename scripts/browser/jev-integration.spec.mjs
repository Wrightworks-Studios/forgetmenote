import {test, expect} from '@playwright/test';
import http from 'node:http';
import https from 'node:https';
import {execFileSync} from 'node:child_process';
import {mkdtemp, readFile, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';
import {openAIControls, saveAIControls} from './ai-controls.mjs';

// TLS and both providers run only against generated notes in a temporary app.
// The browser and Go HTTP/ledger/citation boundaries are not intercepted.
async function providerFixture({onJudgment} = {}) {
  const dir = await mkdtemp(join(tmpdir(), 'jev-browser-provider-'));
  await writeFile(
    join(dir, 'cert.go'),
    `package main
import("crypto/ed25519";"crypto/rand";"crypto/x509";"crypto/x509/pkix";"encoding/pem";"math/big";"net";"os";"path/filepath";"time")
func main(){pub,key,err:=ed25519.GenerateKey(rand.Reader);if err!=nil{panic(err)};tmpl:=&x509.Certificate{SerialNumber:big.NewInt(1),Subject:pkix.Name{CommonName:"Synthetic browser fixture"},NotBefore:time.Now().Add(-time.Hour),NotAfter:time.Now().Add(time.Hour),IPAddresses:[]net.IP{net.ParseIP("127.0.0.1")},KeyUsage:x509.KeyUsageDigitalSignature|x509.KeyUsageCertSign,ExtKeyUsage:[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},IsCA:true,BasicConstraintsValid:true};cert,err:=x509.CreateCertificate(rand.Reader,tmpl,tmpl,pub,key);if err!=nil{panic(err)};raw,err:=x509.MarshalPKCS8PrivateKey(key);if err!=nil{panic(err)};if err=os.WriteFile(filepath.Join(os.Args[1],"cert.pem"),pem.EncodeToMemory(&pem.Block{Type:"CERTIFICATE",Bytes:cert}),0600);err!=nil{panic(err)};if err=os.WriteFile(filepath.Join(os.Args[1],"key.pem"),pem.EncodeToMemory(&pem.Block{Type:"PRIVATE KEY",Bytes:raw}),0600);err!=nil{panic(err)}}`
  );
  execFileSync('go', ['run', join(dir, 'cert.go'), dir]);
  await writeFile(join(dir, 'secret'), 'synthetic-no-real-credential', {mode: 0o600});
  const judgments = [],
    modelRequests = [];
  const judge = https.createServer(
    {key: await readFile(join(dir, 'key.pem')), cert: await readFile(join(dir, 'cert.pem'))},
    async (request, response) => {
      let raw = '';
      for await (const chunk of request) raw += chunk;
      const body = JSON.parse(raw);
      judgments.push(body);
      if (onJudgment && (await onJudgment(body, response))) return;
      const answers = {};
      for (const [id, question] of Object.entries(body.questions)) {
        if (question.type === 'noul') {
          answers[id] = {type: 'noul', noul: id === 'tag0' ? 0.95 : 0.02};
          continue;
        }
        const choices = question.criteria;
        const choice =
          'supported' in choices
            ? 'supported'
            : 'contradiction' in choices
              ? body.state.evidence.length > 1
                ? 'contradiction'
                : 'compatible'
              : 'preserved' in choices
                ? 'partial'
                : 'related';
        const probabilities = Object.fromEntries(Object.keys(choices).map(key => [key, key === choice ? 1 : 0]));
        answers[id] = {type: 'choice', choice, confidence: 1, probabilities};
      }
      response.setHeader('Content-Type', 'application/json');
      response.end(JSON.stringify({model: 'jev-1.13.0', answers, usage: {input_tokens: 60, output_tokens: 20}}));
    }
  );
  const model = http.createServer(async (request, response) => {
    let raw = '';
    for await (const chunk of request) raw += chunk;
    const body = JSON.parse(raw);
    modelRequests.push(body);
    const context = body.messages.map(message => message.content || '').join('\n');
    const match = /"url":"([^"\n]+)"/.exec(context);
    const url = match ? JSON.parse('"' + match[1] + '"') : null;
    response.setHeader('Content-Type', 'application/json');
    response.end(
      JSON.stringify({
        choices: [
          {
            message: {
              role: 'assistant',
              content: url
                ? 'The calibration count is four. [Calibration](' + url + ')'
                : 'No pinned citation was supplied.'
            },
            finish_reason: 'stop'
          }
        ],
        usage: {prompt_tokens: 45, completion_tokens: 12}
      })
    );
  });
  await Promise.all([
    new Promise(resolve => judge.listen(0, '127.0.0.1', resolve)),
    new Promise(resolve => model.listen(0, '127.0.0.1', resolve))
  ]);
  return {
    judgments,
    modelRequests,
    env: {SSL_CERT_FILE: join(dir, 'cert.pem')},
    chatConfig: {
      jev: {
        enabled: true,
        model: 'jev-1.13.0',
        endpoint: 'https://127.0.0.1:' + judge.address().port + '/v1/systemone',
        secretFile: join(dir, 'secret')
      },
      connections: [
        {
          id: 'local',
          label: 'Synthetic model',
          format: 'chat-completions',
          baseURL: 'http://127.0.0.1:' + model.address().port,
          models: ['synthetic'],
          local: true,
          capabilities: {tools: false},
          limits: {runSeconds: 20}
        }
      ]
    },
    async close() {
      await Promise.all([new Promise(resolve => judge.close(resolve)), new Promise(resolve => model.close(resolve))]);
      await rm(dir, {recursive: true, force: true});
    }
  };
}

test('actual automatic citation sidecar reaches the compact badge and prepares an unsent correction', async ({
  page
}) => {
  test.setTimeout(90000);
  page.setDefaultTimeout(10000);
  page.on('pageerror', error => {
    throw error;
  });
  const fixture = await providerFixture();
  let app;
  try {
    app = await startApp({
      name: 'jev-actual-browser-',
      account: {username: 'jev-actual-admin', password: 'DisposableActualJev123!'},
      chatConfig: fixture.chatConfig,
      env: fixture.env
    });
    await app.api('/p/default/api/note', {
      path: 'Calibration.md',
      content: '# Calibration\nThe calibration count is four.\n',
      revision: ''
    });
    await app.login(page, 'password');
    await page.goto(app.origin + '/?project=default&folder=');
    const settings = await openAIControls(page);
    await settings.getByLabel('Check answer citations', {exact: true}).check();
    await saveAIControls(settings);
    await settings.getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-model')).toHaveValue('synthetic');
    await page.locator('#chat-input').fill('What is the calibration count?');
    await page.locator('#chat-send').click();
    try {
      await expect(page.locator('.jev-citations > summary')).toHaveText('Citation checks · 1/1', {timeout: 15000});
    } catch (error) {
      console.log(
        JSON.stringify({
          judgments: fixture.judgments,
          modelRequests: fixture.modelRequests,
          display: await page.locator('.jev-citations').textContent()
        })
      );
      throw error;
    }
    expect(fixture.judgments).toHaveLength(1);
    expect(fixture.judgments[0].state.items[0].evidence[0]).toMatchObject({
      path: 'Calibration.md',
      text: expect.stringContaining('count is four')
    });
    await page.locator('.jev-citations > summary').click();
    await expect(page.locator('.jev-citations')).toContainText('supported');
    await expect(page.locator('.jev-citations blockquote')).toContainText('count is four');
    await page.locator('.jev-citations').getByRole('button', {name: 'Ask chat to propose'}).click();
    await expect(page.locator('#chat-input')).toHaveValue(/do not apply/);
    await expect(page.locator('#chat-input')).toHaveValue(/Calibration.md/);
    expect(fixture.modelRequests).toHaveLength(1);
  } finally {
    await app?.close();
    await fixture.close();
  }
});

test('actual administrator enables and disables reviews for every notebook', async ({page}) => {
  test.setTimeout(90000);
  const fixture = await providerFixture();
  let app;
  try {
    app = await startApp({
      name: 'jev-disable-browser-',
      account: {username: 'jev-disable-admin', password: 'DisposableDisableJev123!'},
      chatConfig: fixture.chatConfig,
      env: fixture.env
    });
    await app.login(page, 'password');
    await page.goto(app.origin + '/?project=default&folder=');
    const admin = await openAIControls(page);
    await admin.getByLabel('Notebook reviews and automatic checks').check();
    await saveAIControls(admin);
    await admin.getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#review-open').click();
    await page.locator('#jev-review-open').click();
    const review = page.getByRole('dialog', {name: 'Review', exact: true});
    await expect(review).toContainText('Automatic checks: Enabled');
    await expect(review.getByRole('button', {name: 'Check now', exact: true})).toBeEnabled();
    await review.getByRole('button', {name: 'Close', exact: true}).click();
    await openAIControls(page);
    await admin.getByLabel('Notebook reviews and automatic checks').uncheck();
    await saveAIControls(admin);
    await admin.getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#review-open').click();
    await page.locator('#jev-review-open').click();
    await expect(review).toContainText('Automatic checks: Disabled');
    await expect(review.getByRole('button', {name: 'Check now', exact: true})).toBeDisabled();
    const notebook = await page.request.get(app.origin + '/p/default/api/jev/settings'),
      personal = await page.request.get(app.origin + '/api/jev/settings');
    expect((await notebook.json()).settings.autoChecks).toBe(false);
    expect((await personal.json()).settings.consent).toBe(false);
    expect(fixture.judgments).toHaveLength(0);
  } finally {
    await app?.close();
    await fixture.close();
  }
});

async function actualReview(page, name, providerOptions) {
  const fixture = await providerFixture(providerOptions);
  let app;
  try {
    app = await startApp({
      name: 'jev-review-' + name + '-',
      account: {username: 'jev-' + name + '-admin', password: 'DisposableReviewJev123!'},
      chatConfig: fixture.chatConfig,
      env: fixture.env
    });
    await app.login(page, 'password');
  } catch (error) {
    await fixture.close();
    throw error;
  }
  const post = async (path, body) => {
    const response = await page.request.post(app.origin + path, {data: body, headers: {Origin: app.origin}});
    expect(response.ok(), await response.text()).toBe(true);
    return response.json();
  };
  const get = async path => {
    const response = await page.request.get(app.origin + path);
    expect(response.ok(), await response.text()).toBe(true);
    return response.json();
  };
  const openChecks = async () => {
    await page.locator('#review-open').click();
    await page.locator('#jev-review-open').click();
    return page.getByRole('dialog', {name: 'Review', exact: true});
  };
  return {
    app,
    fixture,
    post,
    get,
    openChecks,
    async close() {
      await app.close();
      await fixture.close();
    }
  };
}
test('actual consistency job publishes paired notification, marks read and resolves with current guards', async ({
  page
}) => {
  test.setTimeout(90000);
  page.setDefaultTimeout(12000);
  const env = await actualReview(page, 'consistency');
  const {app, fixture, post, get, openChecks} = env;
  try {
    for (const [path, time] of [
      ['Morning.md', '09:00'],
      ['Later.md', '11:00']
    ])
      await app.api('/p/default/api/note', {
        path,
        content: '# Workshop\nThe workshop opens at ' + time + ' on weekdays.\n',
        revision: ''
      });
    await page.goto(app.origin + '/?project=default&folder=');
    let dialog = await openChecks();
    await expect(dialog.getByRole('button', {name: 'Check now', exact: true})).toBeDisabled();
    expect(fixture.judgments).toHaveLength(0);
    await dialog.getByRole('button', {name: 'Close', exact: true}).click();
    const denied = await page.request.post(app.origin + '/p/default/api/jev/jobs', {
      data: {kind: 'consistency', operationId: 'consent-off'},
      headers: {Origin: app.origin}
    });
    expect(denied.ok()).toBe(false);
    expect(fixture.judgments).toHaveLength(0);
    await post('/api/jev/settings', {consent: true, rerank: false, citations: false});
    dialog = await openChecks();
    await dialog.getByRole('button', {name: 'Check now', exact: true}).click();
    await expect(dialog).toContainText('consistency · completed', {timeout: 20000});
    await expect(dialog).toContainText('Possible contradiction');
    expect(fixture.judgments).toHaveLength(3);
    await dialog.getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#notifications-open').click();
    const notices = page.getByRole('dialog', {name: 'Notifications', exact: true});
    await expect(notices).toContainText('Possible contradiction');
    await expect(notices.locator('blockquote')).toHaveCount(2);
    await notices.getByRole('button', {name: 'Mark read', exact: true}).click();
    await expect(page.locator('#notifications-count')).toBeHidden();
    const notification = (await get('/api/notifications')).items[0];
    expect(notification.read).toBe(true);
    await notices.getByRole('button', {name: 'Resolve', exact: true}).click();
    await expect(notices).toContainText('No notifications.');
    const review = await get('/p/default/api/knowledge/review');
    expect(review.findings.find(f => f.comment.id === notification.findingId).comment.resolved).toBe(true);
  } finally {
    await env.close();
  }
});
test('actual extraction audit uses an explicitly selected archived revision and every chosen destination', async ({
  page
}) => {
  test.setTimeout(90000);
  page.setDefaultTimeout(12000);
  const env = await actualReview(page, 'audit');
  const {app, post, get, openChecks} = env;
  try {
    const source =
      '---\nfmn-container: archive\n---\n# Workshop\nThe workshop opens at 09:00 on weekdays except public holidays.\n';
    const saved = await app.api('/p/default/api/note', {path: 'Archived.md', content: source, revision: ''});
    await app.api('/p/default/api/note', {
      path: 'Archived.md',
      content: source + '\nLater context was added.\n',
      revision: saved.revision
    });
    for (const path of [
      'One.md',
      'Two.md',
      'Three.md',
      'Four.md',
      'Five.md',
      'Six.md',
      'Seven.md',
      'Eight.md',
      'Nine.md'
    ])
      await app.api('/p/default/api/note', {
        path,
        content: '# Workshop\nThe workshop opens on weekdays.\n',
        revision: ''
      });
    await post('/api/jev/settings', {consent: true, rerank: false, citations: false});
    await page.goto(app.origin + '/?project=default&folder=');
    const dialog = await openChecks();
    await dialog.getByRole('button', {name: 'Choose sources and destinations'}).click();
    await dialog.getByLabel('Source collection').selectOption('archives');
    await dialog.getByRole('button', {name: 'Previous versions · Archived.md', exact: true}).click();
    await dialog.getByRole('button', {name: new RegExp('^' + saved.revision.slice(0, 7) + ' · ')}).click();
    for (const path of [
      'One.md',
      'Two.md',
      'Three.md',
      'Four.md',
      'Five.md',
      'Six.md',
      'Seven.md',
      'Eight.md',
      'Nine.md'
    ])
      await dialog.getByLabel(path, {exact: true}).check();
    const started = page.waitForResponse(
      response =>
        response.request().method() === 'POST' && new URL(response.url()).pathname === '/p/default/api/jev/jobs'
    );
    await dialog.getByRole('button', {name: 'Start audit', exact: true}).click();
    const response = await started;
    expect(response.ok(), await response.text()).toBe(true);
    expect(response.request().postDataJSON().sources).toEqual([{path: 'Archived.md', revision: saved.revision}]);
    const job = await response.json();
    await expect(dialog).toContainText('audit · completed', {timeout: 20000});
    await expect(dialog).toContainText('Extraction: uncertain');
    const result = (await get('/p/default/api/jev/jobs/' + job.id)).results[0];
    expect(result.complete).toBe(true);
    expect(result.compared).toBe(9);
    expect(result.details).toMatchObject({
      completedBatches: 5,
      totalBatches: 5,
      outcomes: {partial: 5},
      evidenceShown: 8,
      evidenceTotal: 10
    });
    await expect(dialog).toContainText('Showing 8 of 10 evidence excerpts · 9 destination passages checked');
    expect(result.evidence[0].revision).toBe(saved.revision);
    const destination = await get('/p/default/api/note?path=One.md');
    expect(destination.content).toBe('# Workshop\nThe workshop opens on weekdays.\n');
    await dialog.getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#notifications-open').click();
    await expect(page.getByRole('dialog', {name: 'Notifications', exact: true})).toContainText(
      'Showing 8 of 10 evidence excerpts · 9 destination passages checked'
    );
  } finally {
    await env.close();
  }
});
test('actual organization proposes only chosen tags and changes the note after Review acceptance', async ({page}) => {
  test.setTimeout(90000);
  page.setDefaultTimeout(12000);
  const env = await actualReview(page, 'organization');
  const {app, post, get, openChecks} = env;
  try {
    const content = '# Workshop\nThe workshop opens at 09:00 on weekdays.\n';
    await app.api('/p/default/api/note', {path: 'Workshop.md', content, revision: ''});
    await post('/api/jev/settings', {consent: true, rerank: false, citations: false});
    await page.goto(app.origin + '/?project=default&folder=');
    const dialog = await openChecks();
    await dialog.getByRole('button', {name: 'Choose notes and tags'}).click();
    await dialog.getByRole('button', {name: 'Select · Workshop.md', exact: true}).click();
    await dialog.getByLabel('Allowed tags').fill('workshop');
    await dialog.getByRole('button', {name: 'Start organization review'}).click();
    await expect(dialog).toContainText('organization · completed', {timeout: 20000});
    await expect(dialog).toContainText('Tag suggestions');
    expect((await get('/p/default/api/note?path=Workshop.md')).content).toBe(content);
    await dialog.getByRole('button', {name: 'Inspect proposed changes'}).click();
    const review = page.locator('#knowledge-dialog');
    await expect(review).toContainText('Add suggested tags to Workshop.md');
    await review.getByRole('button', {name: 'Accept group', exact: true}).click();
    await expect(review).toContainText('applied');
    const changed = await get('/p/default/api/note?path=Workshop.md');
    expect(changed.content).toContain('workshop');
    expect(changed.content).toContain('The workshop opens at 09:00');
    const state = await get('/p/default/api/knowledge/review');
    expect(state.proposals[0].status).toBe('applied');
  } finally {
    await env.close();
  }
});

async function actualPagedReview(page, name) {
  const env = await actualReview(page, name);
  const {app, post, get} = env;
  try {
    const content = Array.from(
      {length: 51},
      (_, i) => '# Section ' + i + '\nWorkshop procedure ' + i + ' requires a cooling step before packing.\n'
    ).join('\n');
    const source = await app.api('/p/default/api/note', {path: 'Many.md', content, revision: ''});
    await app.api('/p/default/api/note', {
      path: 'Destination.md',
      content: '# Workshop\nWorkshop procedures require packing.\n',
      revision: ''
    });
    await post('/api/jev/settings', {consent: true, rerank: false, citations: false});
    const job = await post('/p/default/api/jev/jobs', {
      kind: 'audit',
      operationId: 'paging-' + name,
      sources: [{path: 'Many.md', revision: source.revision}],
      destinations: ['Destination.md']
    });
    await expect
      .poll(
        async () => {
          const state = await get('/p/default/api/jev/jobs/' + job.id);
          if (state.state === 'paused') await post('/p/default/api/jev/jobs/' + job.id + '/continue', {});
          return state.state;
        },
        {timeout: 45000}
      )
      .toBe('completed');
    const first = await get('/p/default/api/jev/jobs/' + job.id);
    expect(first.resultTotal).toBe(51);
    expect(first.results).toHaveLength(50);
    expect(first.resultNext).toBe(50);
    return {...env, jobID: job.id};
  } catch (error) {
    await env.close();
    throw error;
  }
}
test('actual paged job results load explicitly and preserve earlier results after refresh', async ({page}) => {
  test.setTimeout(90000);
  const env = await actualPagedReview(page, 'jobpages');
  const {app, openChecks} = env;
  try {
    await page.goto(app.origin + '/?project=default&folder=');
    const dialog = await openChecks();
    await dialog.getByRole('button', {name: /^audit · completed/}).click();
    await expect(dialog.locator('.jev-card')).toHaveCount(50);
    const first = await dialog.locator('.jev-card').first().textContent();
    await expect(dialog.getByRole('button', {name: 'Load more results', exact: true})).toBeVisible();
    await dialog.getByRole('button', {name: 'Load more results', exact: true}).click();
    await expect(dialog.locator('.jev-card')).toHaveCount(51);
    await expect(dialog).toContainText('Showing 51 of 51 results');
    expect(await dialog.locator('.jev-card').first().textContent()).toBe(first);
    await dialog.getByRole('button', {name: 'Refresh', exact: true}).click();
    await expect(dialog.locator('.jev-card')).toHaveCount(51);
    await expect(dialog.getByRole('button', {name: 'Load more results', exact: true})).toHaveCount(0);
    await dialog.getByRole('button', {name: 'All checks', exact: true}).click();
    await dialog.getByRole('button', {name: /^audit · completed/}).click();
    await expect(dialog.locator('.jev-card')).toHaveCount(50);
    const delayed = await delayActualPage(page, '**/api/jev/jobs/' + env.jobID + '?offset=50&limit=50');
    await dialog.getByRole('button', {name: 'Load more results', exact: true}).click();
    await delayed.started;
    await dialog.getByRole('button', {name: 'All checks', exact: true}).click();
    await expect(dialog.getByRole('button', {name: 'Check now', exact: true})).toBeVisible();
    await delayed.release();
    await expect(dialog.locator('.jev-card')).toHaveCount(0);
    await dialog.getByRole('button', {name: /^audit · completed/}).click();
    await expect(dialog.locator('.jev-card')).toHaveCount(50);
    const note = await env.get('/p/default/api/note?path=Many.md');
    await app.api('/p/default/api/note', {
      path: 'Many.md',
      content: note.content + '\nChanged after review.\n',
      revision: note.revision
    });
    await dialog.getByRole('button', {name: 'Load more results', exact: true}).click();
    await expect(dialog.locator('.jev-card')).toHaveCount(0);
    await expect(dialog).toContainText('Showing 0 of 0 results');
  } finally {
    await env.close();
  }
});
test('actual paged notifications retain earlier cards and all-item unread count after marking read', async ({page}) => {
  test.setTimeout(90000);
  const env = await actualPagedReview(page, 'noticepages');
  const {app} = env;
  try {
    await page.goto(app.origin + '/?project=default&folder=');
    await expect(page.locator('#notifications-count')).toHaveText('51');
    await page.locator('#notifications-open').click();
    const dialog = page.getByRole('dialog', {name: 'Notifications', exact: true});
    await expect(dialog.locator('[data-notification]')).toHaveCount(50);
    const first = await dialog.locator('[data-notification]').first().getAttribute('data-notification');
    await expect(dialog.getByRole('button', {name: 'Load more notifications', exact: true})).toBeVisible();
    await dialog.getByRole('button', {name: 'Load more notifications', exact: true}).click();
    await expect(dialog.locator('[data-notification]')).toHaveCount(51);
    await expect(dialog).toContainText('Showing 51 of 51 notifications');
    await expect(dialog.locator('[data-notification="' + first + '"]')).toHaveCount(1);
    await dialog.locator('[data-notification]').last().getByRole('button', {name: 'Mark read', exact: true}).click();
    await expect(page.locator('#notifications-count')).toHaveText('50');
    await expect(dialog.locator('[data-notification]')).toHaveCount(51);
    await dialog.getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#notifications-open').click();
    await expect(dialog.locator('[data-notification]')).toHaveCount(50);
    const delayed = await delayActualPage(page, '**/api/notifications?offset=50&limit=50');
    await dialog.getByRole('button', {name: 'Load more notifications', exact: true}).click();
    await delayed.started;
    await dialog.getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#notifications-open').click();
    await expect(dialog.locator('[data-notification]')).toHaveCount(50);
    await delayed.release();
    await expect(dialog.locator('[data-notification]')).toHaveCount(50);
  } finally {
    await env.close();
  }
});

for (const feature of ['instance', 'Quicknotes'])
  for (const localOnly of [false, true])
    test(
      'actual persisted ' + feature + ' consent can be withdrawn when ' + (localOnly ? 'local-only' : 'unavailable'),
      async ({page}) => {
        test.setTimeout(90000);
        const env = await actualReview(page, 'withdraw-' + feature.toLowerCase() + '-' + localOnly);
        const {app, fixture, post, get} = env;
        try {
          if (feature === 'instance')
            await post('/api/organization/action/set_ai_settings', {
              config: {revision: 0, rerank: true, citations: true, reviews: true}
            });
          else await post('/api/jev/settings', {consent: true, rerank: true, citations: true});
          const quick = await get('/api/quicknotes/settings');
          await post('/api/quicknotes/settings', {...quick, cloudConsent: true});
          await writeFile(
            join(app.dir, 'chat.json'),
            JSON.stringify({...fixture.chatConfig, jev: {enabled: false}, localOnly})
          );
          await app.restart();
          await app.login(page, 'password');
          await page.goto(app.origin + '/?project=default&folder=');
          expect((await get('/api/jev/settings')).available).toBe(false);
          if (feature === 'instance') {
            const dialog = await openAIControls(page);
            for (const id of ['rerank', 'citations', 'reviews']) {
              const control = dialog.locator('#instance-ai-' + id);
              await expect(control).toBeChecked();
              await expect(control).toBeEnabled();
              await control.uncheck();
            }
            await saveAIControls(dialog);
            for (const id of ['rerank', 'citations', 'reviews']) {
              await expect(dialog.locator('#instance-ai-' + id)).not.toBeChecked();
              await expect(dialog.locator('#instance-ai-' + id)).toBeDisabled();
            }
            expect((await get('/api/jev/settings')).settings.consent).toBe(false);
          } else {
            await page.locator('#inbox-open').click();
            const dialog = page.locator('#inbox-view');
            await dialog.getByRole('button', {name: 'Preferences', exact: true}).click();
            const consent = dialog.getByLabel('Allow TypeSafe to read saved Quicknotes for sorting');
            await expect(consent).toBeChecked();
            await expect(consent).toBeEnabled();
            await consent.uncheck();
            await dialog.getByLabel('Sorting mode').selectOption('manual');
            await dialog.getByRole('button', {name: 'Save Quicknotes preferences'}).click();
            await expect(dialog).toContainText('Quicknotes preferences saved.');
            expect((await get('/api/quicknotes/settings')).cloudConsent).toBe(false);
            await dialog.getByRole('button', {name: 'Preferences', exact: true}).click();
            await expect(consent).not.toBeChecked();
            await expect(consent).toBeDisabled();
          }
          expect(fixture.judgments).toHaveLength(0);
        } finally {
          await env.close();
        }
      }
    );

async function delayActualPage(page, pattern) {
  let release, entered;
  const gate = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (entered = resolve));
  const response = page.waitForResponse(response => response.url().includes('offset=50&limit=50'));
  await page.route(
    pattern,
    async route => {
      const result = await route.fetch();
      entered();
      await gate;
      await route.fulfill({response: result});
    },
    {times: 1}
  );
  return {
    started,
    async release() {
      release();
      await (await response).finished();
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    }
  };
}

test('actual Stop permanently cancels a check and offers a new review instead of Continue', async ({page}) => {
  test.setTimeout(90000);
  let release,
    entered,
    first = true;
  const gate = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (entered = resolve));
  const env = await actualReview(page, 'cancel-ui', {
    onJudgment: async (body, response) => {
      if (!first) return false;
      first = false;
      entered();
      await gate;
      return response.destroyed;
    }
  });
  const {app, post, get, openChecks} = env;
  try {
    await app.api('/p/default/api/note', {
      path: 'Workshop.md',
      content: '# Workshop\nWorkshop opens at 09:00.\n',
      revision: ''
    });
    await post('/api/jev/settings', {consent: true, rerank: false, citations: false});
    await page.goto(app.origin + '/?project=default&folder=');
    const dialog = await openChecks();
    const created = page.waitForResponse(
      response =>
        response.request().method() === 'POST' && new URL(response.url()).pathname === '/p/default/api/jev/jobs'
    );
    await dialog.getByRole('button', {name: 'Check now', exact: true}).click();
    const stopped = await (await created).json();
    await started;
    await dialog.getByRole('button', {name: 'Stop check', exact: true}).click();
    await expect(dialog).toContainText('consistency · cancelled');
    release();
    await expect(dialog.getByRole('button', {name: 'Continue check', exact: true})).toHaveCount(0);
    await expect(dialog).toContainText('Start a new review to check the notebook again.');
    await dialog.getByRole('button', {name: 'Start new review', exact: true}).click();
    await expect(dialog.getByRole('button', {name: 'Check now', exact: true})).toBeEnabled();
    const recreated = page.waitForResponse(
      response =>
        response.request().method() === 'POST' && new URL(response.url()).pathname === '/p/default/api/jev/jobs'
    );
    await dialog.getByRole('button', {name: 'Check now', exact: true}).click();
    const fresh = await (await recreated).json();
    expect(fresh.id).not.toBe(stopped.id);
    await expect(dialog).toContainText('consistency · completed', {timeout: 15000});
    expect((await get('/p/default/api/jev/jobs/' + stopped.id)).state).toBe('cancelled');
  } finally {
    release();
    await env.close();
  }
});
test('actual paused check can still Continue after a transient provider failure', async ({page}) => {
  test.setTimeout(90000);
  let fail = true;
  const env = await actualReview(page, 'paused-ui', {
    onJudgment: async (body, response) => {
      if (!fail) return false;
      fail = false;
      response.writeHead(503);
      response.end('Synthetic transient provider failure');
      return true;
    }
  });
  const {app, fixture, post, get, openChecks} = env;
  try {
    await app.api('/p/default/api/note', {
      path: 'Workshop.md',
      content: '# Workshop\nWorkshop opens at 09:00.\n',
      revision: ''
    });
    await post('/api/jev/settings', {consent: true, rerank: false, citations: false});
    await page.goto(app.origin + '/?project=default&folder=');
    const dialog = await openChecks();
    const created = page.waitForResponse(
      response =>
        response.request().method() === 'POST' && new URL(response.url()).pathname === '/p/default/api/jev/jobs'
    );
    await dialog.getByRole('button', {name: 'Check now', exact: true}).click();
    const job = await (await created).json();
    await expect(dialog).toContainText('consistency · paused', {timeout: 15000});
    await dialog.getByRole('button', {name: 'Continue check', exact: true}).click();
    await expect(dialog).toContainText('consistency · completed', {timeout: 15000});
    expect((await get('/p/default/api/jev/jobs/' + job.id)).state).toBe('completed');
    expect(fixture.judgments).toHaveLength(2);
  } finally {
    await env.close();
  }
});

test('organizer distinguishes preview and apply and provides guarded Undo on mobile', async ({page}) => {
  test.setTimeout(90000);
  const fixture = await providerFixture();
  let app;
  try {
    app = await startApp({
      name: 'organizer-browser-',
      account: {username: 'organizer-admin', password: 'DisposableOrganizer123!'},
      chatConfig: fixture.chatConfig,
      env: fixture.env
    });
    await app.api('/p/default/api/note', {
      path: 'Workshop.md',
      content: '# Workshop\nThe workshop opens at 09:00.\n',
      revision: ''
    });
    await app.login(page, 'password');
    await page.request.post(app.origin + '/api/jev/settings', {
      data: {consent: true, rerank: false, citations: false},
      headers: {Origin: app.origin}
    });
    await page.goto(app.origin + '/?project=default&folder=');
    await page.locator('#review-open').click();
    await page.locator('#jev-review-open').click();
    const dialog = page.getByRole('dialog', {name: 'Review', exact: true});
    await dialog.getByRole('button', {name: 'Organize existing notes', exact: true}).click();
    await expect(dialog.getByLabel('Move verified duplicates to Trash')).not.toBeChecked();
    await dialog.getByLabel('Tag vocabulary').fill('workshop');
    await dialog.getByRole('button', {name: 'Preview suggestions', exact: true}).click();
    await expect(dialog).toContainText('organize · completed', {timeout: 15000});
    let note = await app.api('/p/default/api/note?path=Workshop.md');
    expect(note.content).not.toContain('tags:');
    await dialog.getByRole('button', {name: 'All checks', exact: true}).click();
    await dialog.getByRole('button', {name: 'Organize existing notes', exact: true}).click();
    await dialog.getByLabel('Tag vocabulary').fill('workshop');
    await dialog.getByRole('button', {name: 'Apply eligible changes', exact: true}).click();
    await expect(dialog).toContainText('organize · completed', {timeout: 15000});
    note = await app.api('/p/default/api/note?path=Workshop.md');
    expect(note.content).toContain('workshop');
    await page.setViewportSize({width: 390, height: 844});
    expect(await dialog.evaluate(n => n.scrollWidth <= n.clientWidth)).toBe(true);
    await dialog.getByRole('button', {name: 'Undo run', exact: true}).click();
    await expect(dialog).toContainText('undone');
    note = await app.api('/p/default/api/note?path=Workshop.md');
    expect(note.content).toBe('# Workshop\nThe workshop opens at 09:00.\n');
  } finally {
    await app?.close();
    await fixture.close();
  }
});

test('organizer duplicate cleanup survives restart and automatic settings remain independent', async ({page}) => {
  test.setTimeout(90000);
  page.setDefaultTimeout(12000);
  const fixture = await providerFixture({
    onJudgment(body, response) {
      if (!body.questions.assessment?.criteria?.redundant) return false;
      const choice = body.state.duplicate.text === body.state.retained.text ? 'redundant' : 'unique';
      response.setHeader('Content-Type', 'application/json');
      response.end(
        JSON.stringify({
          model: 'jev-1.13.0',
          answers: {
            assessment: {
              type: 'choice',
              choice,
              confidence: 1,
              probabilities: {
                redundant: choice === 'redundant' ? 1 : 0,
                unique: choice === 'unique' ? 1 : 0,
                versioned: 0,
                uncertain: 0
              }
            }
          },
          usage: {input_tokens: 60, output_tokens: 20}
        })
      );
      return true;
    }
  });
  let app;
  try {
    app = await startApp({
      name: 'organizer-restart-',
      account: {username: 'organizer-restart', password: 'DisposableOrganizer123!'},
      chatConfig: fixture.chatConfig,
      env: fixture.env
    });
    const shared = '# Clock\nThe Juniper calibration requires seven turns.\n';
    for (const path of ['a.md', 'b.md']) await app.api('/p/default/api/note', {path, content: shared, revision: ''});
    await app.login(page, 'password');
    const post = async (path, body) => {
      const r = await page.request.post(app.origin + path, {data: body, headers: {Origin: app.origin}});
      expect(r.ok()).toBeTruthy();
      return r.json();
    };
    const get = async path => (await page.request.get(app.origin + path)).json();
    await post('/api/jev/settings', {consent: true});
    await page.goto(app.origin + '/?project=default&folder=');
    await page.locator('#review-open').click();
    await page.locator('#jev-review-open').click();
    const dialog = page.getByRole('dialog', {name: 'Review', exact: true});
    await dialog.getByRole('button', {name: 'Organize existing notes', exact: true}).click();
    await dialog.getByLabel('Add tags', {exact: true}).uncheck();
    await dialog.getByLabel('Move verified duplicates to Trash').check();
    await dialog.getByRole('button', {name: 'Apply eligible changes', exact: true}).click();
    await expect(dialog).toContainText('organize · completed', {timeout: 30000});
    await expect(dialog.getByRole('link', {name: 'Inspect Trash entry'})).toBeVisible();
    const id = (await get('/p/default/api/jev/jobs')).jobs.find(j => j.kind === 'organize').id,
      paid = fixture.judgments.length;
    await app.restart();
    await page.reload();
    await page.locator('#review-open').click();
    await page.locator('#jev-review-open').click();
    await dialog.getByRole('button', {name: /^organize · completed/}).click();
    await dialog.getByRole('button', {name: 'Undo run', exact: true}).click();
    await expect(dialog).toContainText('undone');
    expect((await get('/p/default/api/note?path=b.md')).content).toBe(shared);
    expect(fixture.judgments).toHaveLength(paid);
    await dialog.getByRole('button', {name: 'All checks', exact: true}).click();
    await expect(dialog.getByRole('button', {name: 'Automatic organization settings', exact: true})).toHaveCount(0);
    // Existing API consumers retain their persisted policy until instance adoption.
    const automatic = await get('/p/default/api/jev/organization');
    expect(automatic.settings.enabled).toBe(false);
    await post('/p/default/api/jev/organization', {
      enabled: true,
      generation: automatic.settings.generation,
      policy: {tags: true, vocabulary: ['calibration']}
    });
    expect((await get('/p/default/api/jev/settings')).settings.autoChecks).toBe(false);
    expect((await get('/p/default/api/jev/organization')).settings.enabled).toBe(true);
    expect(fixture.judgments).toHaveLength(paid);
    // The automatic run after its two-minute quiet period is covered in Go by
    // TestOrganizerAutomaticBaselineDebounceAndIndependentSettings.
    expect((await get('/p/default/api/jev/jobs/' + id)).results.some(r => r.status === 'undone')).toBe(true);
  } finally {
    await app?.close();
    await fixture.close();
  }
});
