import AxeBuilder from '@axe-core/playwright';
import { randomUUID } from 'node:crypto';
import { manage } from '../helpers/management';
import { signInGatewayOwner } from './signIn';
import { expect, test } from '../playwright';

test('FOCUS dataset totals expose exact amounts and incomplete coverage', async ({
  page
}) => {
  await signInGatewayOwner(page);
  const created = await manage<{ export_sink_id: string }>(
    page,
    'POST',
    '/api/v1/observability/sinks',
    {
      idempotency: randomUUID(),
      body: {
        name: 'Commitment browser fixture',
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
  await page.route(
    `**/api/v1/observability/sinks/${created.body.export_sink_id}/focus-datasets*`,
    (route) =>
      route.fulfill({
        headers: { 'Cache-Control': 'no-store' },
        json: {
          items: [
            {
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
            }
          ],
          next_cursor: null
        }
      })
  );
  await page.goto('/settings');
  const panel = page.locator('.observability-panel');
  const sinkRow = panel
    .getByRole('row')
    .filter({ hasText: 'Commitment browser fixture' })
    .first();
  await sinkRow.getByRole('button', { name: 'Datasets' }).click();
  const table = panel.locator('.focus-datasets');
  await expect(table).toContainText('Purchased: 12.345678901234');
  await expect(table).toContainText('Unused: 12.345678901234');
  await expect(table).toContainText('Provisional · Incomplete coverage');
  await expect(
    table.getByRole('columnheader', { name: 'Commitments' })
  ).toBeVisible();
  expect(
    (await new AxeBuilder({ page }).include('.focus-datasets').analyze())
      .violations
  ).toEqual([]);
});
