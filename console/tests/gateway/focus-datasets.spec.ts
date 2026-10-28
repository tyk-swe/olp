import AxeBuilder from '@axe-core/playwright';
import { randomUUID } from 'node:crypto';
import type { FocusDataset } from '../../src/lib/features/observability/api';
import { manage } from '../helpers/management';
import { signInGatewayOwner } from './signIn';
import { expect, test } from '../playwright';

test('FOCUS dataset pages preserve order, totals and sink isolation through retry', async ({
  page
}) => {
  await signInGatewayOwner(page);
  const createSink = async (name: string) => {
    const created = await manage<{ export_sink_id: string }>(
      page,
      'POST',
      '/api/v1/observability/sinks',
      {
        idempotency: randomUUID(),
        body: {
          name,
          type: 's3',
          destination: 'https://objects.example.test/commitments',
          streams: ['focus'],
          format: 'csv_gzip',
          enabled: false,
          settings: { cost_basis: 'cost' }
        }
      }
    );
    expect(created.status).toBe(201);
    return created.body.export_sink_id;
  };
  const sinkId = await createSink('Commitment browser fixture');
  const otherSinkId = await createSink('Other dataset browser fixture');
  const dataset: FocusDataset = {
    charge_date: '2026-10-01',
    revision: 2,
    generated_at: '2026-10-02T03:00:00Z',
    delivered_at: '2026-10-02T03:01:00Z',
    row_count: 2,
    part_count: 1,
    billed_cost: '12.345678901234',
    effective_cost: '12.345678901234',
    attempts: 1,
    last_error_code: null,
    current: true,
    commitment_totals: {
      purchased_cost: '12.345678901234',
      allocated_cost: '0',
      unused_cost: '12.345678901234',
      provisional: true,
      complete: false
    }
  };
  const days = Array.from({ length: 51 }, (_, index) => ({
    ...dataset,
    charge_date: new Date(Date.UTC(2026, 9, 1 - index))
      .toISOString()
      .slice(0, 10)
  }));
  const older: FocusDataset = {
    ...days[50],
    revision: 3,
    delivered_at: null,
    current: false,
    billed_cost: '8.25',
    commitment_totals: {
      purchased_cost: '8.25',
      allocated_cost: '3.25',
      unused_cost: '5',
      provisional: false,
      complete: true
    }
  };
  const other = { ...dataset, charge_date: '2026-06-01' };
  const cursor = Buffer.from(days[49].charge_date).toString('base64url');
  const path = `**/api/v1/observability/sinks/${sinkId}/focus-datasets*`;
  const pending = Promise.withResolvers<void>();
  await page.route(path, async (route) => {
    const query = new URL(route.request().url()).searchParams;
    expect(query.get('limit')).toBe('50');
    if (!query.has('cursor')) {
      await route.fulfill({
        headers: { 'Cache-Control': 'no-store' },
        json: { items: days.slice(0, 50), next_cursor: cursor }
      });
      return;
    }
    expect(query.get('cursor')).toBe(cursor);
    await pending.promise;
    await route.fulfill({
      status: 500,
      headers: { 'Cache-Control': 'no-store' },
      json: { title: 'Dataset page unavailable', status: 500 }
    });
  });
  await page.route(
    `**/api/v1/observability/sinks/${otherSinkId}/focus-datasets*`,
    (route) => {
      expect(new URL(route.request().url()).searchParams.has('cursor')).toBe(
        false
      );
      return route.fulfill({
        headers: { 'Cache-Control': 'no-store' },
        json: { items: [other], next_cursor: null }
      });
    }
  );
  await page.goto('/settings');
  const panel = page.locator('.observability-panel');
  const toggle = (name: string) =>
    panel
      .getByRole('row')
      .filter({ hasText: name })
      .first()
      .getByRole('button', { name: 'Datasets', exact: true });
  const table = panel.locator('.focus-datasets');
  const dates = table.locator('tbody tr td:first-child');
  const olderButton = panel.getByRole('button', { name: 'Older datasets' });
  const newerButton = panel.getByRole('button', { name: 'Newer datasets' });
  await toggle('Commitment browser fixture').click();
  await expect(dates).toHaveText(
    days.slice(0, 50).map((day) => day.charge_date)
  );
  await expect(table).toContainText('Purchased: 12.345678901234');
  await expect(table).toContainText('Unused: 12.345678901234');
  await expect(table).toContainText('Provisional · Incomplete coverage');
  await expect(newerButton).toHaveCount(0);
  await olderButton.click();
  await expect(panel.getByRole('status')).toHaveText('Loading datasets…');
  await expect(newerButton).toBeDisabled();
  await expect(table).toHaveCount(0);
  await toggle('Other dataset browser fixture').click();
  await expect(dates).toHaveText([other.charge_date]);
  pending.resolve();
  await expect(newerButton).toHaveCount(0);
  await expect(olderButton).toHaveCount(0);
  await toggle('Commitment browser fixture').click();
  await expect(dates).toHaveText(
    days.slice(0, 50).map((day) => day.charge_date)
  );
  await olderButton.click();
  await expect(panel.getByRole('alert')).toHaveText(
    'Focus datasets are unavailable.'
  );
  await expect(newerButton).toBeEnabled();
  await page.route(path, (route) => {
    expect(new URL(route.request().url()).searchParams.get('cursor')).toBe(
      cursor
    );
    return route.fulfill({
      headers: { 'Cache-Control': 'no-store' },
      json: { items: [older], next_cursor: null }
    });
  });
  await panel.getByRole('button', { name: 'Retry datasets' }).click();
  await expect(dates).toHaveText([older.charge_date]);
  await expect(table.locator('tbody tr td')).toHaveText([
    older.charge_date,
    '3',
    'pending',
    '2',
    '8.25',
    'Purchased: 8.25 Allocated: 3.25 Unused: 5 Closed · Complete',
    'Restatement pending'
  ]);
  await expect(olderButton).toHaveCount(0);
  await expect(panel.getByRole('alert')).toHaveCount(0);
  await newerButton.click();
  await expect(dates).toHaveText(
    days.slice(0, 50).map((day) => day.charge_date)
  );
  await olderButton.click();
  await expect(dates).toHaveText([older.charge_date]);
  await expect(
    table.getByRole('columnheader', { name: 'Commitments' })
  ).toBeVisible();
  expect(
    (
      await new AxeBuilder({ page })
        .include('.focus-datasets')
        .include('[aria-label="Dataset pages"]')
        .analyze()
    ).violations
  ).toEqual([]);
  await toggle('Other dataset browser fixture').click();
  await expect(dates).toHaveText([other.charge_date]);
  await toggle('Other dataset browser fixture').click();
  await expect(table).toHaveCount(0);
  await toggle('Other dataset browser fixture').click();
  await expect(dates).toHaveText([other.charge_date]);
});
