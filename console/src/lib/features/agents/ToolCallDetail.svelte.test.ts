import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountWithState, setFieldValue } from '#test/mount.js';
import ToolCallDetailPage from './ToolCallDetailPage.svelte';

it('resolves held amounts from the call screen and shows the resulting recorded outcome', async () => {
  const resolve = vi.fn();
  const evidence = 'c'.repeat(64);
  serve(
    async (request) => {
      if (request.method === 'POST') {
        resolve(await request.json());
        return json({ id: 'call', status: 'settled' });
      }
      return json({
        id: 'call',
        status: resolve.mock.calls.length ? 'settled' : 'outcome_unknown',
        remaining_cost: resolve.mock.calls.length ? '0' : '2'
      });
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(ToolCallDetailPage, { callID: 'call' });
  await eventually(() => expect(target.querySelector('form')).not.toBeNull());
  const inputs = target.querySelectorAll('input');
  setFieldValue(inputs[0], evidence);
  setFieldValue(inputs[1], '0.123456789012');
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() =>
    expect(resolve).toHaveBeenCalledWith({
      outcome: 'settled',
      evidence_digest: evidence,
      observed_cost: '0.123456789012',
      observed_charge: '0'
    })
  );
  await eventually(() =>
    expect(target.textContent).toContain('Outcome settled')
  );
  expect(target.querySelector('form')).toBeNull();
  expect(target.textContent).toContain('"remaining_cost": "0"');
});
