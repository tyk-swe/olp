import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const stage = process.argv[2];
assert.ok(stage === 'before' || stage === 'after');
const require = createRequire(resolve('console/package.json'));
const { chromium } = require('@playwright/test');
const origin = 'http://127.0.0.1:46162';
const browser = await chromium.launch();
try {
  for (const fixture of [
    {
      name: 'browser',
      button: 'Authorize in browser',
      link: 'Sign in upstream',
      href: '/fixture-authorize',
      payload: { authorization_url: '/fixture-authorize' }
    },
    {
      name: 'device',
      button: 'Use device authorization',
      link: 'the authorization page',
      href: '/fixture-device',
      payload: {
        device: {
          verification_url: '/fixture-device',
          user_code: 'EXAMPLE',
          interval: 5,
          expires_at: '2030-01-01T00:00:00Z'
        }
      }
    }
  ]) {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
    page.on('pageerror', (error) => console.error(error));
    await page.route('**/api/**', (route) => {
      const path = new URL(route.request().url()).pathname;
      const body = {
        '/api/v1/sessions/current': {
          user: {
            id: 'fixture-owner',
            email: 'owner@example.test',
            display_name: 'Fixture Owner',
            role: 'owner',
            access_scope: 'global'
          },
          operations: ['read', 'keys', 'self'],
          csrf_token: 'fixture-csrf'
        },
        '/api/v1/auth/capabilities': {
          local_login_enabled: true,
          oidc_login_enabled: false,
          saml_login_enabled: false
        },
        '/api/v1/branding': { name: 'OpenLLMProxy' },
        '/api/v1/mcp-grant-enrollments/claim': {
          enrollment: { id: 'fixture-grant' },
          state: 'pending'
        },
        '/api/v1/mcp-grant-enrollments/fixture-grant/authorize': {
          state: 'pending',
          ...fixture.payload
        }
      }[path] ?? { items: [] };
      return route.fulfill({ json: body });
    });
    await page.goto(`${origin}/agents/grants#fixture-grant.fixture-token`);
    await page.getByRole('button', { name: 'Claim enrollment', exact: true }).click();
    await page.getByRole('button', { name: fixture.button, exact: true }).click();
    const link = page.getByRole('link', { name: fixture.link, exact: true });
    if (stage === 'before') {
      await link.waitFor();
      assert.equal(await link.getAttribute('href'), fixture.href);
      console.log(`${fixture.name}: ${fixture.link} (${fixture.href})`);
    } else {
      const message = 'The personal grant authorization response is invalid';
      await page.getByRole('alert').filter({ hasText: message }).waitFor();
      assert.equal(await link.count(), 0);
      console.log(`${fixture.name}: ${message}, authorization link absent`);
    }
    await page.getByRole('heading', { name: 'Authorize personal access' }).evaluate((heading) => {
      heading.scrollIntoView({ block: 'center' });
    });
    await page.screenshot({
      path: fileURLToPath(new URL(`${stage}-${fixture.name}.png`, import.meta.url)),
      animations: 'disabled'
    });
    await page.close();
  }
} finally {
  await browser.close();
}
