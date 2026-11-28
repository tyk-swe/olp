import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const [worktree, revision] = process.argv.slice(2);
assert(worktree && ['before', 'after'].includes(revision));
const require = createRequire(resolve(worktree, 'console/package.json'));
const { chromium, expect } = require('@playwright/test');
const proof = dirname(fileURLToPath(import.meta.url));
const url = 'http://127.0.0.1:15157/usage?start=2026-10-01T00%3A00%3A00.000Z&end=2026-10-01T02%3A00%3A00.000Z&dimension=route&granularity=hour';
const counters = {
  cached_input_tokens: '0',
  cache_write_input_tokens: '0',
  cache_write_5m_input_tokens: '0',
  cache_write_1h_input_tokens: '0',
  estimated_input_tokens: '0',
  reported_input_tokens: '0',
  media_units: '0',
  audio_input_tokens: '0',
  cached_audio_input_tokens: '0',
  audio_output_tokens: '0',
  image_input_tokens: '0',
  cached_image_input_tokens: '0',
  image_output_tokens: '0',
  allocated_commitment: '0',
  currency: 'USD',
  incomplete_count: 0,
  unpriced_count: 0,
  estimated_attempt_count: 0,
  uncharged_attempt_count: 0
};
const first = {
  ...counters,
  bucket: '2026-10-01T00:00:00Z',
  request_count: 12,
  input_tokens: '1000',
  output_tokens: '200',
  estimated_cost: '0.015',
  charge: '0.015'
};
const second = {
  ...counters,
  bucket: '2026-10-01T01:00:00Z',
  request_count: 28,
  input_tokens: '2400',
  output_tokens: '500',
  estimated_cost: '0.042',
  charge: '0.042'
};
const coverage = {
  range_complete: true,
  approximate: false,
  excluded_partial_aggregate_boundaries: 0
};
const fixtures = {
  '/api/v1/sessions/current': {
    user: {
      id: 'a1000000-0000-4000-8000-000000000001',
      email: 'operator@example.test',
      display_name: 'Local operator',
      role: 'owner',
      access_scope: 'global'
    },
    csrf_token: 'local-browser-fixture',
    session_id: 'a1000000-0000-4000-8000-000000000002',
    operations: ['usage']
  },
  '/api/v1/auth/capabilities': {
    local_login_enabled: true,
    oidc_login_enabled: false
  },
  '/api/v1/branding': {},
  '/api/v1/usage/time-series': { items: [first, second], coverage },
  '/api/v1/usage/breakdown': {
    items: [{ ...second, dimension: 'fixture-chat' }],
    coverage
  },
  '/api/v1/usage/unpriced-breakdown': { items: [], coverage },
  '/api/v1/usage/summary': {
    ...counters,
    request_count: 40,
    input_tokens: '3400',
    output_tokens: '700',
    estimated_cost: '0.057',
    charge: '0.057',
    coverage,
    request_metadata_consumer: {
      state: 'healthy',
      pending_events: 0,
      lag_events: 0
    },
    request_metadata_gap_events: 0,
    uncertain_request_metadata_gap_count: 0,
    complete: true
  }
};

const browser = await chromium.launch();
try {
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1000 },
    timezoneId: 'UTC',
    locale: 'en-US',
    reducedMotion: 'reduce'
  });
  page.setDefaultTimeout(10000);
  page.on('pageerror', (error) => console.error(error));
  await page.route('**/api/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (Object.hasOwn(fixtures, path)) {
      await route.fulfill({ json: fixtures[path] });
      return;
    }
    console.error(`Unexpected fixture request: ${path}`);
    await route.fulfill({ status: 404, json: { title: 'Fixture route missing' } });
  });
  await page.goto(url);
  const scrubber = page.getByRole('slider', { name: 'Inspect usage bucket' });
  const figure = page.getByRole('figure', { name: 'Requests per hour' });
  const chart = figure.locator('.chart');
  const readout = figure.locator('[aria-live="polite"]');
  await expect(figure).toBeVisible().catch(async (error) => {
    console.error(await page.locator('body').innerText());
    throw error;
  });
  await figure.scrollIntoViewIfNeeded();

  for (let presses = 0; presses < 100 && !(await scrubber.evaluate((input) => input.matches(':focus'))); presses += 1) {
    await page.keyboard.press('Tab');
  }
  await expect(scrubber).toBeFocused();
  await figure.scrollIntoViewIfNeeded();
  const bounds = await chart.boundingBox();
  assert(bounds);
  const x = bounds.x + 52;
  await page.mouse.move(x, bounds.y + bounds.height / 2);
  await expect(readout).toContainText('12 req');
  await page.keyboard.press('ArrowRight');
  await expect(readout).toContainText('28 req');
  await expect(scrubber).toHaveAttribute('aria-valuetext', /28 requests/);

  const initialReadout = (await readout.innerText()).replace(/\s+/g, ' ');
  await page.mouse.move(x, bounds.y - 8);
  await expect(scrubber).toBeFocused();
  if (revision === 'before') {
    await expect(readout).toHaveText('Hover or focus chart to inspect buckets');
    await expect(scrubber).toHaveAttribute('aria-valuetext', 'No bucket selected');
  } else {
    await expect(readout).toContainText('28 req');
    await expect(scrubber).toHaveAttribute('aria-valuetext', /28 requests/);
  }
  await figure.screenshot({ path: resolve(proof, `${revision}.png`), animations: 'disabled' });
  console.log(page.url());
  console.log(`Keyboard selection: ${initialReadout}`);
  console.log(`After pointer leaves (scrubber still focused): ${(await readout.innerText()).replace(/\s+/g, ' ')}`);
  console.log(`aria-valuetext: ${await scrubber.getAttribute('aria-valuetext')}`);
  await page.keyboard.press('ArrowRight');
  await expect(scrubber).toHaveAttribute('aria-valuetext', /requests$/);
  await page.keyboard.press('Tab');
  await expect(scrubber).not.toBeFocused();
  await expect(readout).toHaveText('Hover or focus chart to inspect buckets');
  console.log('After Tab moves focus away: selection cleared');

  await page.mouse.move(x, bounds.y + bounds.height / 2);
  await expect(readout).toContainText('12 req');
  await page.mouse.move(x, bounds.y - 8);
  await expect(readout).toHaveText('Hover or focus chart to inspect buckets');
  console.log('Pointer-only hover and leave: selection cleared');
} finally {
  await browser.close();
}
