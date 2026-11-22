import { flushSync, mount, unmount } from 'svelte';
import { expect, it, onTestFinished, vi } from 'vitest';
import ApiKeyTable from './ApiKeyTable.svelte';
import type { ApiKey } from './api';

const active: ApiKey = {
  id: 'active-id',
  lookup_id: 'active-lookup',
  name: 'Active',
  etag: '018f1234-5678-7abc-8def-0123456789ab',
  project_id: null,
  project_name: null,
  budget_group_id: null,
  created_by: 'owner',
  created_by_email: 'owner@example.test',
  created_at: '2025-01-01T00:00:00Z',
  allow_provider_state: false,
  allowed_attribution_keys: [],
  allowed_routes: [],
  response_metadata: false,
  regional_limits: {},
  scopes: ['inference'],
  budget: {
    daily: {
      accrued: '0',
      limit: null,
      window_ends_at: '2025-01-02T00:00:00Z'
    },
    monthly: {
      accrued: '0',
      limit: null,
      window_ends_at: '2025-02-01T00:00:00Z'
    },
    unpriced_attempts: 0
  }
};
const paused: ApiKey = {
  ...active,
  id: 'paused-id',
  lookup_id: 'paused-lookup',
  name: 'Paused',
  paused_at: '2025-01-01T01:00:00Z'
};

function button(row: Element, label: string) {
  return [...row.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === label
  );
}

it('marks paused keys and offers the matching pause or resume action', () => {
  const target = document.createElement('div');
  const onPause = vi.fn();
  const onResume = vi.fn();
  const component = mount(ApiKeyTable, {
    target,
    props: {
      items: [active, paused],
      canManage: true,
      limitsEnforced: true,
      gatewayAvailable: true,
      busy: false,
      busyKey: '',
      onRotate: vi.fn(),
      onPause,
      onResume,
      onRevoke: vi.fn()
    }
  });
  onTestFinished(() => unmount(component));
  const [activeRow, pausedRow] = target.querySelectorAll('tbody tr');
  expect(activeRow.querySelector('.badge')?.textContent).toBe('active');
  expect(pausedRow.querySelector('.badge')?.textContent).toBe('paused');
  expect(button(activeRow, 'Resume')).toBeUndefined();
  expect(button(pausedRow, 'Pause')).toBeUndefined();
  button(activeRow, 'Pause')?.click();
  button(pausedRow, 'Resume')?.click();
  flushSync();
  expect(onPause).toHaveBeenCalledExactlyOnceWith(active);
  expect(onResume).toHaveBeenCalledExactlyOnceWith(paused);
});
