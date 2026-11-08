import { setupProgress } from '#lib/features/overview/setupProgress.js';
import { describe, expect, it } from 'vitest';
import type { Grant } from '#lib/features/access/session/authorization.js';
import {
  MANAGEMENT_REQUIREMENTS,
  type ManagementOperation
} from '#lib/management/requirements.js';
import {
  activeTab,
  breadcrumbs,
  NAVIGATION_SECTIONS,
  owningItem,
  paletteEntries,
  PROFILE,
  SETTINGS,
  visibleSections
} from '#lib/shell/navigation.js';

const everyOperation = [
  ...new Set(
    Object.values(MANAGEMENT_REQUIREMENTS).flatMap((requirement) =>
      requirement.alternatives.flatMap((alternative) => alternative.operations)
    )
  )
] as ManagementOperation[];

const fullGrant: Grant = { operations: everyOperation, access_scope: 'global' };
const items = [
  ...NAVIGATION_SECTIONS.flatMap((section) => section.items),
  SETTINGS
];
const pages = items.flatMap((item) =>
  (item.tabs ?? [item]).map((leaf) => [item.label, leaf.href] as const)
);
const hrefs = (grant: Grant, gatewayAvailable: boolean) =>
  visibleSections(grant, gatewayAvailable).flatMap((section) =>
    section.items.flatMap(({ href, tabs }) => [
      href,
      ...tabs.map((tab) => tab.href)
    ])
  );

describe('navigation', () => {
  it.each(pages)('highlights %s on %s and its detail pages', (label, href) => {
    expect(owningItem(href)?.label).toBe(label);
    expect(owningItem(`${href}/detail`)?.label).toBe(label);
  });

  it.each([false, true])(
    'keeps setup links aligned with navigation when completed is %s',
    (complete) => {
      const progress = setupProgress({
        loading: false,
        failed: false,
        activeProvider: true,
        enabledModels: complete,
        activeRoute: true,
        apiKey: complete,
        grant: fullGrant
      });
      const entries = paletteEntries(fullGrant, true);
      const destination = (label: string) =>
        entries.find((entry) => entry.label === label)?.href;

      expect(`/${progress.steps[2].href}`).toBe(destination('Models'));
      expect(`/${progress.steps[4].href}`).toBe(
        destination(complete ? 'API keys' : 'Create an API key')
      );
    }
  );

  it('gives pages nested under another item to their own owner', () => {
    expect(owningItem('/settings/profile')).toBe(PROFILE);
    expect(owningItem('/agents/grants')?.label).toBe('My connections');
    expect(owningItem('/agents/toolsets')?.label).toBe('Agents and tools');
  });

  it('marks the deepest matching tab as active', () => {
    const usage = items.find((item) => item.label === 'Usage');
    expect(activeTab('/usage/routing', usage?.tabs ?? [])?.label).toBe(
      'Routing savings'
    );
    expect(activeTab('/usage', usage?.tabs ?? [])?.label).toBe('Summary');
  });

  it('opens an item on the first tab the grant can read', () => {
    const members = visibleSections(
      { operations: ['access_read'], access_scope: 'global' },
      true
    )
      .flatMap((section) => section.items)
      .find(({ item }) => item.label === 'Members');

    expect(members?.href).toBe(members?.tabs[0]?.href);
    expect(members?.tabs.length).toBeGreaterThan(0);
  });

  it('hides gateway pages when the gateway is unavailable', () => {
    const available = hrefs(fullGrant, false);

    expect(available).toContain('/plugins');
    expect(available).toContain('/keys');
    expect(available).not.toContain('/providers');
    expect(available).not.toContain('/activity/runs');
  });

  it('names the section, page and tab in breadcrumbs', () => {
    expect(breadcrumbs('/pricing/discounts').map(({ label }) => label)).toEqual(
      ['Spend', 'Pricing', 'Discounts']
    );
    expect(breadcrumbs('/')).toEqual([]);
  });

  it('only offers create actions the grant can perform', () => {
    const labels = (grant: Grant) =>
      paletteEntries(grant, true)
        .filter((entry) => entry.group === 'Create')
        .map(({ label }) => label);

    expect(labels(fullGrant)).toContain('Create an API key');
    expect(labels({ operations: ['read'], access_scope: 'global' })).toEqual(
      []
    );
  });
});
