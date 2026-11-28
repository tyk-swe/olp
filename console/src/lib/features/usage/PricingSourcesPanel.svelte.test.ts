import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  mountWithState,
  reportedError,
  setFieldValue
} from '#test/mount.js';
import PricingSourcesPanel from './PricingSourcesPanel.svelte';

const source = {
  id: 'source-1',
  name: 'Example price list',
  format: 'prices',
  url: 'https://prices.example.test/list.json',
  enabled: true,
  etag: 'etag-1',
  updated_at: '2026-01-01T00:00:00Z'
};
const snapshot = {
  id: 'snapshot-1',
  sha256: 'a'.repeat(64),
  price_count: 1,
  currency: 'USD',
  fetched_at: '2026-01-01T00:00:00Z',
  document: {}
};

it('reports a refused publication at the input that holds it', async () => {
  const published = vi.fn();
  serve(
    async (request) => {
      if (request.url.includes('/snapshots?'))
        return json({ items: [snapshot], next_cursor: null });
      if (request.url.endsWith('/refresh'))
        return json({
          snapshot,
          diff: { added_count: 0, changed_count: 0, removed_count: 0 },
          skipped: []
        });
      if (request.url.endsWith('/publish')) {
        published(await request.json());
        return json(
          {
            title: 'Invalid request',
            status: 422,
            errors: {
              effective_at: [
                {
                  code: 'invalid',
                  message: 'Choose an effective time now or in the future.'
                }
              ]
            }
          },
          422
        );
      }
      return json({ items: [source], next_cursor: null });
    },
    ['usage', 'settings']
  );
  const { target } = mountWithState(PricingSourcesPanel, {}, { attach: true });
  const button = (label: string) =>
    eventually(() => {
      const found = findButton(target, label);
      expect(found).toBeDefined();
      return found!;
    });
  const input = (label: string) =>
    target.querySelector<HTMLInputElement>(`input[aria-label="${label}"]`)!;

  (await button('Refresh')).click();
  (await button('Publish…')).click();
  const publish = await button('Publish');
  setFieldValue(input('Overrides JSON'), '{"model":"example"}');
  publish.click();

  await eventually(() =>
    expect(document.activeElement).toBe(input('Overrides JSON'))
  );
  expect(reportedError(input('Overrides JSON'))).toBe(
    'Overrides must be a JSON array of price entries.'
  );
  expect(published).not.toHaveBeenCalled();

  setFieldValue(input('Overrides JSON'), '[]');
  publish.click();

  await eventually(() =>
    expect(reportedError(input('Effective at'))).toBe(
      'Choose an effective time now or in the future.'
    )
  );
  expect(reportedError(input('Overrides JSON'))).toBeUndefined();
  expect(target.querySelector('[role="alert"]')).toBeNull();
  expect(published).toHaveBeenCalledExactlyOnceWith({
    effective_at: null,
    overrides: []
  });
});
