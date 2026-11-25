import {execFileSync} from 'node:child_process';
import {mkdtemp, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startPostgres} from './postgres.mjs';
import {buildNoNetlink} from './no-netlink.mjs';

// One binary and one Postgres cluster per invocation; every app still owns
// independent mutable data, a database and ports. Always build from this
// checkout so an old environment cannot select it.
export default async function setup() {
  const dir = await mkdtemp(join(tmpdir(), 'forgetmenote-browser-build-'));
  let postgres;
  try {
    const binary = join(dir, 'app');
    // A change to a widely imported package recompiles most of the binary. On a
    // 2-core runner shared with the image build and smokes, that can take more
    // than a minute; the bound only stops a hung toolchain.
    execFileSync('go', ['build', '-trimpath', '-o', binary, './cmd/forgetmenote'], {timeout: 600000});
    // Workers load playwright.config.mjs after setup, so the browser fixture sees it.
    const noNetlink = buildNoNetlink(dir);
    if (noNetlink) process.env.FMN_BROWSER_NO_NETLINK = noNetlink;
    postgres = await startPostgres();
    process.env.FMN_BROWSER_BINARY = binary;
    process.env.FMN_BROWSER_CLIPBOARD_LOCK = join(dir, 'clipboard.lock');
    process.env.FMN_BROWSER_DATABASE_HOST = postgres.host;
    return async () => {
      try {
        await postgres.stop();
      } finally {
        await rm(dir, {recursive: true, force: true});
      }
    };
  } catch (error) {
    await postgres?.stop();
    await rm(dir, {recursive: true, force: true});
    throw error;
  }
}
