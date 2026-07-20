import { defineConfig, devices } from '@playwright/test';

const databaseUrl = process.env.OLP_CONSOLE_E2E_DATABASE_URL;
const masterKeyFile = process.env.OLP_CONSOLE_E2E_MASTER_KEY_FILE;
const authHmacKeyFile = process.env.OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE;
const bootstrapTokenFile = process.env.OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE;
if (!databaseUrl) {
  throw new Error('OLP_CONSOLE_E2E_DATABASE_URL is required for the Rust-hosted console integration');
}
if (!masterKeyFile) {
  throw new Error('OLP_CONSOLE_E2E_MASTER_KEY_FILE is required for invitation integration');
}
if (!authHmacKeyFile) {
  throw new Error('OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE is required for control-plane authentication');
}
if (!bootstrapTokenFile) {
  throw new Error('OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE is required for first-run setup');
}

export default defineConfig({
  testDir: './tests/integration',
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? 'test-results',
  fullyParallel: false,
  forbidOnly: true,
  // The test mutates a real database. Retrying against that same database
  // would no longer prove the first-run setup path and could mask a failure.
  retries: 0,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',
  use: {
    // Browsers apply the Secure exception for localhost. This lets the test
    // prove that real cookie storage accepts the __Host- contract while the
    // Rust listener remains loopback-only.
    baseURL: 'http://localhost:4175',
    reducedMotion: 'reduce',
    trace: 'retain-on-failure'
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: [
    {
      command: 'node tests/integration/mock-oidc.mjs',
      url: 'http://127.0.0.1:4176/.well-known/openid-configuration',
      reuseExistingServer: false,
      timeout: 10_000
    },
    {
      command: 'cargo run --manifest-path ../Cargo.toml --locked -p olp -- migrate && cargo run --manifest-path ../Cargo.toml --locked -p olp -- control',
      url: 'http://127.0.0.1:4177/health/live',
      reuseExistingServer: false,
      timeout: 180_000,
      env: {
        ...process.env,
        OLP_DATABASE_URL: databaseUrl,
        OLP_LISTEN_ADDR: '127.0.0.1:4175',
        OLP_OBSERVABILITY_LISTEN_ADDR: '127.0.0.1:4177',
        OLP_PUBLIC_ORIGIN: 'http://localhost:4175',
        OLP_CONSOLE_DIR: 'build',
        OLP_MASTER_KEY_FILE: masterKeyFile,
        OLP_AUTH_HMAC_KEY_FILE: authHmacKeyFile,
        OLP_BOOTSTRAP_TOKEN_FILE: bootstrapTokenFile,
        OLP_ALLOW_INSECURE_OIDC_FOR_TESTS: 'test-only'
      }
    }
  ]
});
