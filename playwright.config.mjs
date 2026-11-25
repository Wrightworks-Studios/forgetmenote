import {defineConfig} from '@playwright/test';
import {browserEnvironment} from './scripts/browser/no-netlink.mjs';
const workers = Number(process.env.FMN_BROWSER_WORKERS || 2);
if (!Number.isInteger(workers) || workers < 1 || workers > 4) throw Error('FMN_BROWSER_WORKERS must be 1–4');
export default defineConfig({
  testDir:'./scripts/browser', globalSetup:'./scripts/browser/setup.mjs', workers,
  // Files own their fixtures; cases sharing those fixtures stay sequential.
  fullyParallel:false, retries:0, timeout:60000,
  grep:process.env.FMN_BROWSER_GREP ? new RegExp(process.env.FMN_BROWSER_GREP) : undefined,
  outputDir:process.env.FMN_BROWSER_RESULTS || 'test-results',
  use:{headless:true,trace:'retain-on-failure',screenshot:'only-on-failure',launchOptions:{executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
    // Linux: Chromium ignores host interface changes, such as Docker containers started beside the suite.
    env:browserEnvironment(process.env.FMN_BROWSER_NO_NETLINK)}},
  reporter:[['list'],['json',{outputFile:process.env.FMN_BROWSER_REPORT || 'test-results/report.json'}]],
});
