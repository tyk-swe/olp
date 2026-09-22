import { defineConfig, devices } from '@playwright/test';

if (!process.env.OLP_DATABASE_URL || !process.env.OLP_VALKEY_URL) {
  throw new Error('Run make integration to provision isolated services');
}

function database(name: string) {
  const url = new URL(process.env.OLP_DATABASE_URL!);
  url.pathname =
    '/' + (process.env.OLP_CONSOLE_E2E_DATABASE_PREFIX ?? '') + name;
  return url.toString();
}

export default defineConfig({
  testDir: './tests',
  testMatch: '**/{access,foundation,gateway}/**/*.spec.ts',
  timeout: 90_000,
  outputDir: 'test-results/go-access',
  workers: 1,
  retries: 0,
  forbidOnly: true,
  reporter: 'list',
  use: {
    ...devices['Desktop Chrome'],
    actionTimeout: 10_000,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure'
  },
  projects: [
    { name: 'go-packaged', use: { baseURL: 'http://127.0.0.1:4182' } },
    { name: 'go-vite', use: { baseURL: 'http://127.0.0.1:4183' } }
  ],
  webServer: [
    {
      command: './tests/journeys/run-olp.sh',
      url: 'http://127.0.0.1:9182/health/live',
      reuseExistingServer: false,
      env: {
        OLP_CONSOLE_E2E_BACKEND: 'go',
        OLP_DATABASE_URL: database('olp_go_packaged'),
        OLP_PUBLIC_ORIGIN: 'http://127.0.0.1:4182',
        OLP_LISTEN_ADDR: '127.0.0.1:4182',
        OLP_OBSERVABILITY_LISTEN_ADDR: '127.0.0.1:9182',
        OLP_CONSOLE_DIR: 'console/build',
        OLP_PROVIDER_EGRESS_ALLOW_CIDRS: '127.0.0.0/8',
        OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS: '127.0.0.1'
      }
    },
    {
      command: './tests/journeys/run-olp.sh',
      url: 'http://127.0.0.1:9184/health/live',
      reuseExistingServer: false,
      env: {
        OLP_CONSOLE_E2E_BACKEND: 'go',
        OLP_DATABASE_URL: database('olp_go_vite'),
        OLP_PUBLIC_ORIGIN: 'http://127.0.0.1:4183',
        OLP_LISTEN_ADDR: '127.0.0.1:4184',
        OLP_OBSERVABILITY_LISTEN_ADDR: '127.0.0.1:9184',
        OLP_CONSOLE_DIR: 'console/build',
        OLP_PROVIDER_EGRESS_ALLOW_CIDRS: '127.0.0.0/8',
        OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS: '127.0.0.1'
      }
    },
    {
      command: 'node tests/access/mock-oidc.mjs',
      url: 'http://127.0.0.1:4186/.well-known/openid-configuration',
      reuseExistingServer: false
    },
    {
      command: 'node tests/journeys/mock-azure-openai.mjs',
      url: 'http://127.0.0.1:4178/health',
      reuseExistingServer: false
    },
    {
      command: 'node tests/gateway/mock-openai.mjs',
      url: 'http://127.0.0.1:4187/health',
      reuseExistingServer: false
    },
    {
      command: 'pnpm dev --host 127.0.0.1 --port 4183 --strictPort',
      url: 'http://127.0.0.1:4183',
      reuseExistingServer: false,
      env: { OLP_DEV_API_ORIGIN: 'http://127.0.0.1:4184' }
    }
  ]
});
