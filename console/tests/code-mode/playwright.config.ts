import { fileURLToPath } from 'node:url';
import { defineConfig } from '@playwright/test';
import base from '../../playwright.config';

const cwd = fileURLToPath(new URL('../..', import.meta.url));
const servers = Array.isArray(base.webServer) ? base.webServer : [];

export default defineConfig({
  ...base,
  testDir: '.',
  testMatch: '*.spec.ts',
  outputDir: '../../test-results/code-mode',
  projects: [{ name: 'packaged', use: { baseURL: 'http://127.0.0.1:4182' } }],
  webServer: servers.slice(0, 1).map((server) => ({ ...server, cwd }))
});
