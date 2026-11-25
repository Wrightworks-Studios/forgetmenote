import {execFileSync} from 'node:child_process';
import {join} from 'node:path';

const source = new URL('./no-netlink.c', import.meta.url).pathname;

// On Linux, builds the library that stops Chromium reacting to host interface
// changes (see no-netlink.c) and returns its path; other platforms need none.
export function buildNoNetlink(dir) {
  if (process.platform !== 'linux') return '';
  const library = join(dir, 'no-netlink.so');
  try {
    execFileSync('cc', ['-shared', '-fPIC', '-O2', '-Wall', '-Werror', '-o', library, source, '-ldl'], {
      timeout: 30000,
      stdio: ['ignore', 'ignore', 'pipe']
    });
  } catch (error) {
    throw Error('Browser tests on Linux need a C compiler (cc) to build ' + source + ': ' + error.message);
  }
  return library;
}

// Browser launch environment carrying the library, scoped to Chromium only.
export function browserEnvironment(library) {
  if (!library) return undefined;
  return {...process.env, LD_PRELOAD: [library, process.env.LD_PRELOAD].filter(Boolean).join(' ')};
}
