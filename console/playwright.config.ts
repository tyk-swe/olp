import { defineConfig, devices } from '@playwright/test';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
const vaultDirectory =
  process.env.OLP_CONSOLE_E2E_VAULT_DIR ??
  join(tmpdir(), 'olp-vault-browser-fixture');

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
  testMatch: '**/{access,basics,gateway,plugins,code-mode,fleet}/**/*.spec.ts',
  timeout: 90_000,
  outputDir: 'test-results/access',
  workers: 1,
  retries: 0,
  forbidOnly: true,
  reporter: 'list',
  use: {
    ...devices['Desktop Chrome'],
    // Entrance motion is decorative; accessibility scans and text assertions
    // must never sample a half-faded frame.
    reducedMotion: 'reduce',
    actionTimeout: 10_000,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure'
  },
  projects: [
    {
      name: 'packaged',
      // WebAuthn requires a DNS relying-party ID; the Vite origin is localhost.
      testIgnore: ['**/vite-smoke.spec.ts', '**/access/mfa.spec.ts'],
      use: { baseURL: 'http://127.0.0.1:4182' }
    },
    {
      name: 'vite',
      testMatch: '**/basics/{shell,vite-smoke}.spec.ts',
      use: { baseURL: 'http://localhost:4183' }
    },
    {
      // Signing in completes setup, which the Vite smoke test walks itself.
      name: 'vite-mfa',
      testMatch: '**/access/mfa.spec.ts',
      dependencies: ['vite'],
      use: { baseURL: 'http://localhost:4183' }
    }
  ],
  webServer: [
    {
      command: 'go run ../tests/samlfixture',
      url: 'http://127.0.0.1:4196/metadata',
      reuseExistingServer: false
    },
    {
      command: './tests/journeys/run-olp.sh',
      url: 'http://127.0.0.1:9182/health/live',
      reuseExistingServer: false,
      env: {
        OLP_DATABASE_URL: database('olp_packaged'),
        OLP_VAULT_ADDR: 'http://127.0.0.1:4199',
        OLP_VAULT_ROLE: 'olp',
        OLP_VAULT_JWT_FILE: join(vaultDirectory, 'identity.jwt'),
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
        OLP_DATABASE_URL: database('olp_vite'),
        OLP_VAULT_ADDR: 'http://127.0.0.1:4199',
        OLP_VAULT_ROLE: 'olp',
        OLP_VAULT_JWT_FILE: join(vaultDirectory, 'identity.jwt'),
        OLP_PUBLIC_ORIGIN: 'http://localhost:4183',
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
    ...(process.env.OLP_CONSOLE_E2E_FLEET_SECRET_DIR
      ? [
          { name: 'first', host: 'localhost', port: 4197 },
          { name: 'second', host: '127.0.0.1', port: 4198 }
        ].map(({ name, host, port }) => ({
          command: 'bash tests/fleet/run-olp.sh',
          url: `http://127.0.0.1:${port + 5000}/health/live`,
          reuseExistingServer: false,
          gracefulShutdown: { signal: 'SIGTERM' as const, timeout: 30_000 },
          env: {
            OLP_DATABASE_URL: database(`olp_fleet_${name}`),
            OLP_PUBLIC_ORIGIN: `http://${host}:${port}`,
            OLP_LISTEN_ADDR: `127.0.0.1:${port}`,
            OLP_OBSERVABILITY_LISTEN_ADDR: `127.0.0.1:${port + 5000}`,
            OLP_CONSOLE_DIR: 'console/build',
            OLP_DATABASE_MAX_CONNECTIONS: '5',
            OLP_CONSOLE_E2E_FLEET_INSTANCE_SECRET_DIR: `${process.env.OLP_CONSOLE_E2E_FLEET_SECRET_DIR}/${name}`
          }
        }))
      : []),
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
      command: 'node tests/gateway/mock-vault.mjs',
      url: 'http://127.0.0.1:4199/health',
      reuseExistingServer: false
    },
    {
      command: 'node tests/gateway/mock-anthropic.mjs',
      url: 'http://127.0.0.1:4188/health',
      reuseExistingServer: false
    },
    {
      command: 'node tests/gateway/mock-native-operations.mjs',
      url: 'http://127.0.0.1:4189/health',
      reuseExistingServer: false
    },
    {
      command: 'node tests/plugins/mock-plugin-upstream.mjs',
      url: 'http://127.0.0.1:4190/health',
      reuseExistingServer: false
    },
    {
      command: 'pnpm dev --host localhost --port 4183 --strictPort',
      url: 'http://localhost:4183',
      reuseExistingServer: false,
      env: { OLP_DEV_API_ORIGIN: 'http://127.0.0.1:4184' }
    }
  ]
});
