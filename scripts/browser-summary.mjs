// Run Playwright and print only counts, failures and artifact paths.
// Arguments pass through, e.g. `node scripts/browser-summary.mjs scripts/browser/editor-drafts.spec.mjs`.
// The complete list output stays in the log file named at the end.
import {spawn} from 'node:child_process';
import {mkdtemp, readFile} from 'node:fs/promises';
import {createWriteStream} from 'node:fs';
import {tmpdir} from 'node:os';
import {join, relative} from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'forgetmenote-browser-summary-'));
const env = {...process.env};
env.FMN_BROWSER_RESULTS ||= join(dir, 'results');
env.FMN_BROWSER_REPORT ||= join(dir, 'report.json');
const log = join(dir, 'playwright.log');
const output = createWriteStream(log);
const child = spawn('npx', ['--no-install', 'playwright', 'test', ...process.argv.slice(2)], {env, stdio: ['ignore', 'pipe', 'pipe']});
child.stdout.pipe(output, {end: false});
child.stderr.pipe(output, {end: false});
const code = await new Promise(resolve => child.on('close', resolve));
output.end();

let report;
try {
  report = JSON.parse(await readFile(env.FMN_BROWSER_REPORT, 'utf8'));
} catch (error) {
  console.log(`Playwright exited ${code} without a JSON report (${error.message}). Log: ${log}`);
  process.exit(code || 1);
}
const plain = text => String(text || '').replace(/\x1b\[[0-9;]*m/g, '');
const failures = [];
(function walk(suite) {
  for (const spec of suite.specs || []) for (const test of spec.tests || []) {
    if (test.status !== 'unexpected' && test.status !== 'flaky') continue;
    const result = test.results.find(item => item.status !== 'passed') || test.results.at(-1);
    const artifact = result?.attachments?.find(item => item.path)?.path;
    failures.push({status: test.status, where: `${spec.file}:${spec.line}`, title: spec.title,
      error: plain(result?.error?.message).split('\n').filter(Boolean).slice(0, 12).join('\n    '),
      artifact: artifact ? relative(process.cwd(), join(artifact, '..')) : ''});
  }
  for (const child of suite.suites || []) walk(child);
})({suites: report.suites});

const stats = report.stats || {};
console.log(`${stats.expected || 0} passed, ${stats.unexpected || 0} failed, ${stats.flaky || 0} flaky, ${stats.skipped || 0} skipped in ${Math.round((stats.duration || 0) / 1000)}s`);
for (const failure of failures) {
  console.log(`\n${failure.status === 'flaky' ? 'FLAKY' : 'FAILED'} ${failure.where} › ${failure.title}\n    ${failure.error}`);
  if (failure.artifact) console.log(`    artifacts: ${failure.artifact}`);
}
console.log(`\nFull log: ${log}`);
process.exit(code);
