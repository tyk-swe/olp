import { describe, expect, it } from 'vitest';
import type { Grant } from '#lib/features/access/session/authorization.js';
import {
  MANAGEMENT_REQUIREMENTS,
  type ManagementOperation
} from '#lib/management/requirements.js';
import {
  isActivePath,
  NAVIGATION_GROUPS,
  navigationEntries
} from '#lib/shell/navigation.js';

const everyOperation = [
  ...new Set(
    Object.values(MANAGEMENT_REQUIREMENTS).flatMap((requirement) =>
      requirement.alternatives.flatMap((alternative) => alternative.operations)
    )
  )
] as ManagementOperation[];

const fullGrant: Grant = { operations: everyOperation, access_scope: 'global' };
const items = NAVIGATION_GROUPS.flatMap((group) => group.items);
const activeLabels = (path: string) =>
  items
    .filter((item) => isActivePath(path, item.href))
    .map(({ label }) => label);

describe('navigation', () => {
  it.each(items.map((item) => [item.label, item.href] as const))(
    'highlights only %s on its own page',
    (label, href) => {
      expect(activeLabels(href)).toEqual([label]);
    }
  );

  it.each(
    items
      .filter((item) => item.href !== '/')
      .map((item) => [item.label, item.href] as const)
  )('keeps %s highlighted on its detail pages', (label, href) => {
    expect(activeLabels(`${href}/detail`)).toEqual([label]);
  });

  it('opens each group on its primary page for a full grant', () => {
    const landing = Object.fromEntries(
      navigationEntries(fullGrant, true).map(({ label, href }) => [label, href])
    );

    expect(landing.Gateway).toBe('/providers');
    expect(landing.Agents).toBe('/agents');
    expect(landing.Operations).toBe('/requests');
  });

  it('surfaces a group trimmed to one page as a direct link to it', () => {
    const entries = navigationEntries(
      { operations: ['access_read'], access_scope: 'global' },
      false
    );

    expect(entries.find(({ href }) => href === '/access')?.label).toBe(
      'Members'
    );
  });

  it('hides gateway pages when the gateway is unavailable', () => {
    const hrefs = navigationEntries(fullGrant, false).flatMap((entry) =>
      entry.items.map(({ href }) => href)
    );

    expect(hrefs).toContain('/plugins');
    expect(hrefs).not.toContain('/providers');
    expect(hrefs).not.toContain('/runs');
  });
});
