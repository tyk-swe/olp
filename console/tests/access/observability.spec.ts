import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import { manage } from '../helpers/management';

let receiver: Server;
let receiverUrl: string;
const received: { path: string; body: string }[] = [];

test.beforeAll(async () => {
  receiver = createServer((req, res) => {
    let body = '';
    req.on('data', (chunk) => (body += chunk));
    req.on('end', () => {
      received.push({ path: req.url ?? '', body });
      res.writeHead(204).end();
    });
  });
  await new Promise<void>((resolve) =>
    receiver.listen(0, '127.0.0.1', resolve)
  );
  const { port } = receiver.address() as AddressInfo;
  receiverUrl = `http://127.0.0.1:${port}`;
});

test.afterAll(async () => {
  await new Promise((resolve) => receiver.close(resolve));
});

const run = `e2e-${Date.now().toString(36)}`;

test('owner manages export sinks, capture policies and notifications through the console', async ({
  page
}, testInfo) => {
  test.setTimeout(240_000);
  await signInGatewayOwner(page);

  const project = await manage(page, 'POST', '/api/v1/projects', {
    body: { name: `E2E capture ${run}` },
    idempotency: crypto.randomUUID()
  });
  expect(project.status).toBe(201);
  const projectId = (project.body as { id: string }).id;
  const key = await manage(page, 'POST', '/api/v1/api-keys', {
    body: {
      name: `Matrix key ${run}`,
      scopes: ['inference'],
      project_id: projectId
    },
    idempotency: crypto.randomUUID()
  });
  expect(key.status).toBe(201);
  const keyId = (key.body as { id: string }).id;

  try {
    await page.goto('/settings');
    const panel = page.locator('.observability-panel');
    await expect(
      panel.getByRole('heading', { name: 'Export sinks and payload capture' })
    ).toBeVisible();

    await panel.locator('#sink-name').fill(`Collector ${run}`);
    await panel.locator('#sink-type').selectOption('https');
    await panel.locator('#sink-destination').fill(`${receiverUrl}/json`);
    await panel.locator('#sink-format').selectOption('json');
    await panel.locator('input[type="checkbox"]').first().check();
    await panel
      .locator('#sink-credential')
      .fill('{"signing_secret":"browser-test-secret"}');
    await panel.getByRole('button', { name: 'Create sink' }).click();
    await expect(
      panel.getByRole('row', { name: new RegExp(`Collector ${run}`) })
    ).toBeVisible();

    await expect(panel.getByText(/idle|unknown/).first()).toBeVisible();
    await panel.getByRole('button', { name: 'Gaps' }).first().click();
    await expect(panel.getByText('No export gaps recorded.')).toBeVisible();

    await panel
      .getByRole('row', { name: new RegExp(`Collector ${run}`) })
      .getByRole('button', { name: 'Edit' })
      .click();
    await panel.locator('#sink-name').fill(`Collector ${run} v2`);

    await panel.locator('#sink-filter-project').fill(projectId);
    await expect(panel.locator('#sink-type')).toBeDisabled();
    await panel.getByRole('button', { name: 'Save sink' }).click();
    await expect(
      panel.getByRole('row', { name: new RegExp(`Collector ${run} v2`) })
    ).toBeVisible();

    await panel.scrollIntoViewIfNeeded();
    await page.screenshot({
      path: testInfo.outputPath('settings.png'),
      fullPage: true
    });
    await testInfo.attach('settings', {
      path: testInfo.outputPath('settings.png'),
      contentType: 'image/png'
    });
    const axe = await new AxeBuilder({ page })
      .include('.observability-panel')
      .analyze();
    expect(axe.violations).toEqual([]);

    await expect(panel.locator('.capture-warning')).toContainText('unredacted');
    const master = panel.getByRole('button', {
      name: /^(Enable|Disable) capture$/
    });
    if ((await master.textContent()) === 'Enable capture') {
      await master.click();
    }
    await expect(
      panel.getByRole('button', { name: 'Disable capture' })
    ).toBeVisible();

    await panel
      .locator('#policy-project')
      .selectOption({ label: `E2E capture ${run}` });
    await panel
      .locator('#policy-sink')
      .selectOption({ label: 'Collector ' + run + ' v2 (https)' });
    await panel.locator('#policy-ratio').fill('0.25');
    await panel.getByLabel('input', { exact: true }).check();
    await panel.getByLabel('output', { exact: true }).check();
    await panel.locator('#policy-keys').fill(keyId);
    await panel.getByRole('button', { name: 'Create capture policy' }).click();
    const policyRow = panel
      .locator('table')
      .last()
      .getByRole('row')
      .filter({ hasText: 'Collector ' + run + ' v2' });
    await expect(policyRow.getByText('0.25', { exact: true })).toBeVisible();

    await policyRow.getByRole('button', { name: 'Edit' }).click();
    await panel.locator('#policy-ratio').fill('0.5');
    await panel.locator('#policy-keys').fill('');
    await panel.getByRole('button', { name: 'Save capture policy' }).click();
    await expect(policyRow.getByText('0.5', { exact: true })).toBeVisible();

    await panel.getByRole('button', { name: 'Disable capture' }).click();
    await expect(
      panel.getByRole('button', { name: 'Enable capture' })
    ).toBeVisible();
    await policyRow.getByRole('button', { name: 'Disable' }).click();
    await expect(
      policyRow.getByRole('button', { name: 'Enable' })
    ).toBeVisible();
    await policyRow.getByRole('button', { name: 'Enable' }).click();
    await expect(panel.locator('[role="alert"]').first()).toBeVisible();
    await expect(
      policyRow.getByRole('button', { name: 'Enable' })
    ).toBeVisible();

    await page.setViewportSize({ width: 320, height: 800 });
    await panel.scrollIntoViewIfNeeded();
    await page.screenshot({
      path: testInfo.outputPath('settings-mobile.png'),
      fullPage: true
    });
    await testInfo.attach('settings-mobile', {
      path: testInfo.outputPath('settings-mobile.png'),
      contentType: 'image/png'
    });
    const mobileWidth = await page.evaluate(
      () => document.documentElement.scrollWidth
    );
    expect(mobileWidth).toBeLessThanOrEqual(320);
    const axeMobile = await new AxeBuilder({ page })
      .include('.observability-panel')
      .analyze();
    expect(axeMobile.violations).toEqual([]);
    await page.setViewportSize({ width: 1280, height: 800 });

    await page.goto('/api-keys');
    const notifications = page.locator('.notifications-panel');
    await expect(
      notifications.getByRole('heading', {
        name: 'Notification destinations and rules'
      })
    ).toBeVisible();

    await notifications.locator('#dest-name').fill(`Webhook ${run}`);
    await notifications.locator('#dest-url').fill(`${receiverUrl}/hook`);
    await notifications.locator('#dest-secret').fill('e2e-signing-secret');
    await notifications
      .locator('#dest-project')
      .selectOption({ label: `E2E capture ${run}` });
    await notifications
      .getByRole('button', { name: 'Create destination' })
      .click();
    await expect(
      notifications.getByRole('row', { name: new RegExp(`Webhook ${run}`) })
    ).toBeVisible();

    await notifications.locator('#rule-name').fill(`Spend ${run}`);
    await notifications.locator('#rule-event').selectOption('report.spend');
    await notifications
      .locator('#rule-project')
      .selectOption({ label: `E2E capture ${run}` });
    await notifications
      .locator('#rule-destination')
      .selectOption({ label: `Webhook ${run}` });
    await notifications.locator('#rule-period').selectOption('weekly');
    await notifications.getByRole('button', { name: 'Create rule' }).click();
    const ruleRow = notifications
      .getByRole('row')
      .filter({ hasText: `Spend ${run}` });
    await expect(ruleRow).toBeVisible();

    await ruleRow.getByRole('button', { name: 'Edit' }).click();
    const editRow = notifications
      .getByRole('row')
      .filter({ has: page.getByLabel('Rule name', { exact: true }) });
    await editRow.getByLabel('Period', { exact: true }).selectOption('monthly');
    await editRow.getByRole('button', { name: 'Save' }).click();
    const savedRow = notifications
      .getByRole('row')
      .filter({ hasText: `Spend ${run}` });
    await expect(savedRow.getByText('monthly')).toBeVisible();
    await savedRow.getByRole('button', { name: 'Disable' }).click();

    await page.screenshot({ path: testInfo.outputPath('notifications.png') });
    await testInfo.attach('notifications', {
      path: testInfo.outputPath('notifications.png'),
      contentType: 'image/png'
    });
    const axe2 = await new AxeBuilder({ page })
      .include('.notifications-panel')
      .analyze();
    expect(axe2.violations).toEqual([]);

    await page.goto(`/requests?session_id=SessionCase&project_id=${projectId}`);
    const download = page.waitForEvent('download');
    await page.getByRole('button', { name: 'Export CSV' }).click();
    expect((await download).suggestedFilename()).toBe('olp-requests.csv');

    await page.goto(`/usage?project_id=${projectId}`);
    const usageDownload = page.waitForEvent('download');
    await page.getByRole('button', { name: 'Export CSV' }).first().click();
    expect((await usageDownload).suggestedFilename()).toBe('olp-usage.csv');
  } finally {
    const sinks = await manage(page, 'GET', '/api/v1/observability/sinks');
    const mine = (
      sinks.body as {
        items: { export_sink_id: string; etag: string; name: string }[];
      }
    ).items.filter((s) => s.name.startsWith(`Collector ${run}`));
    for (const sink of mine) {
      await manage(
        page,
        'PATCH',
        `/api/v1/observability/sinks/${sink.export_sink_id}`,
        { body: { enabled: false }, etag: sink.etag }
      );
    }
    const rules = await manage(page, 'GET', '/api/v1/notifications/rules');
    const spent = (
      rules.body as {
        items: { id: string; etag: string; name: string; enabled: boolean }[];
      }
    ).items.filter((r) => r.name === `Spend ${run}` && r.enabled);
    for (const rule of spent) {
      await manage(page, 'PATCH', `/api/v1/notifications/rules/${rule.id}`, {
        body: { enabled: false },
        etag: rule.etag
      });
    }
    await manage(page, 'PATCH', '/api/v1/observability/capture', {
      body: { enabled: false },
      match: '/api/v1/observability/capture'
    });
  }
});
