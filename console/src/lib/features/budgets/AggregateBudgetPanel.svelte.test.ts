import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findControl,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import AggregateBudgetPanel from './AggregateBudgetPanel.svelte';

const budget = {
  policy: null,
  etag: 'original',
  usage: {
    daily: { accrued: '0' },
    weekly: { accrued: '0' },
    monthly: { accrued: '0' },
    unpriced_attempts: 0
  }
};

it('reports each refused budget limit at the input that holds it', async () => {
  const writes = vi.fn();
  serve(
    async (request) => {
      if (request.method === 'GET') return json(budget);
      writes(await request.json());
      return json(
        {
          title: 'Validation failed',
          status: 422,
          errors: {
            monthly_cost_limit: [
              { code: 'invalid', message: 'This cap is too low.' }
            ]
          }
        },
        422
      );
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(
    AggregateBudgetPanel,
    { projectId: 'project' },
    { attach: true }
  );
  const daily = await eventually(() =>
    findControl(target, 'Daily cost limit (USD)')
  );
  const weekly = findControl(target, 'Weekly cost limit (USD)');
  const monthly = findControl(target, 'Monthly cost limit (USD)');

  setFieldValue(daily, '0');
  setFieldValue(weekly, 'ten');
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(daily));
  expect(reportedError(daily)).toContain('positive decimals');
  expect(reportedError(weekly)).toContain('positive decimals');
  expect(reportedError(monthly)).toBeUndefined();
  expect(writes).not.toHaveBeenCalled();

  setFieldValue(daily, ' 10 ');
  setFieldValue(weekly, '');
  setFieldValue(monthly, '20');
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(monthly));
  expect(reportedError(monthly)).toBe('This cap is too low.');
  expect(reportedError(daily)).toBeUndefined();
  expect(target.querySelector('[role="alert"]')).toBeNull();
  expect(writes).toHaveBeenCalledExactlyOnceWith({
    policy: {
      daily_cost_limit: '10',
      weekly_cost_limit: null,
      monthly_cost_limit: '20',
      cost_basis: 'cost'
    }
  });
});
