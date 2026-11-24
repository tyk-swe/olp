import { expect, it } from 'vitest';
import { activeTab } from '#lib/components/tabLinks.js';

const tabs = [
  { label: 'Summary', href: '/usage' },
  { label: 'Routing savings', href: '/usage/routing' },
  { label: 'Reconciliation', href: '/usage/reconciliation' }
];

it('marks the deepest matching tab as active', () => {
  expect(activeTab('/usage/routing', tabs)?.label).toBe('Routing savings');
  expect(activeTab('/usage', tabs)?.label).toBe('Summary');
});
