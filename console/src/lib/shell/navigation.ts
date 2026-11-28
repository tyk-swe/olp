import { resolve } from '$app/paths';
import { activeTab, isAtOrBelow } from '#lib/components/tabLinks.js';
import { allows, type Grant } from '#lib/session/authorization.js';
import type { IconName } from '#lib/components/icons.js';
import { BUILD_SECTION } from '#lib/shell/navigationTree/build.js';
import { AGENTS_SECTION } from '#lib/shell/navigationTree/agents.js';
import { ACCESS_SECTION } from '#lib/shell/navigationTree/access.js';
import { SPEND_SECTION } from '#lib/shell/navigationTree/spend.js';
import { OBSERVE_SECTION } from '#lib/shell/navigationTree/observe.js';
import {
  OVERVIEW,
  PLAYGROUND,
  PROFILE,
  SETTINGS
} from '#lib/shell/navigationTree/pinned.js';
import type {
  NavigationItem,
  NavigationLeaf,
  NavigationSection
} from '#lib/shell/navigationTree/types.js';

export type VisibleItem = {
  readonly item: NavigationItem;
  readonly href: string;
  readonly tabs: readonly NavigationLeaf[];
};

export type VisibleSection = {
  readonly label: string;
  readonly items: readonly VisibleItem[];
};

export type PaletteEntry = {
  readonly label: string;
  readonly href: string;
  readonly group: string;
  readonly icon: IconName;
};

export const NAVIGATION_SECTIONS: readonly NavigationSection[] = [
  BUILD_SECTION,
  AGENTS_SECTION,
  ACCESS_SECTION,
  SPEND_SECTION,
  OBSERVE_SECTION
];

const CREATE_ACTIONS: readonly (PaletteEntry & NavigationLeaf)[] = [
  {
    label: 'Add a provider',
    href: resolve('providers/new'),
    group: 'Create',
    icon: 'provider',
    route: 'POST /api/v1/providers'
  },
  {
    label: 'Create a route',
    href: resolve('routes/new'),
    group: 'Create',
    icon: 'route',
    route: 'POST /api/v1/route-drafts'
  },
  {
    label: 'Create an API key',
    href: resolve('keys/new'),
    group: 'Create',
    icon: 'key',
    route: 'POST /api/v1/api-keys',
    controlPlane: true
  },
  {
    label: 'Invite a member',
    href: resolve('members/invitations'),
    group: 'Create',
    icon: 'access',
    route: 'POST /api/v1/invitations',
    controlPlane: true
  },
  {
    label: 'Start an investigation',
    href: resolve('investigations/new'),
    group: 'Create',
    icon: 'search',
    route: 'POST /api/v1/investigations'
  }
];

const leafVisible = (
  leaf: NavigationLeaf,
  grant: Grant,
  gatewayAvailable: boolean
) =>
  (gatewayAvailable || leaf.controlPlane === true) &&
  (!leaf.route || allows(grant, leaf.route));

export function visibleItem(
  item: NavigationItem,
  grant: Grant,
  gatewayAvailable: boolean
): VisibleItem | null {
  const tabs = (item.tabs ?? [item]).filter((leaf) =>
    leafVisible(leaf, grant, gatewayAvailable)
  );
  if (!tabs.length) return null;
  return { item, href: tabs[0].href, tabs: item.tabs ? tabs : [] };
}

export function visibleSections(
  grant: Grant,
  gatewayAvailable: boolean
): readonly VisibleSection[] {
  return NAVIGATION_SECTIONS.flatMap((section) => {
    const items = section.items
      .map((item) => visibleItem(item, grant, gatewayAvailable))
      .filter((item): item is VisibleItem => item !== null);
    return items.length ? [{ label: section.label, items }] : [];
  });
}

const ALL_ITEMS: readonly NavigationItem[] = [
  OVERVIEW,
  PLAYGROUND,
  ...NAVIGATION_SECTIONS.flatMap((section) => section.items),
  SETTINGS,
  PROFILE
];

const sectionOf = (item: NavigationItem) =>
  NAVIGATION_SECTIONS.find((section) => section.items.includes(item))?.label;

export function owningItem(path: string): NavigationItem | undefined {
  const scored = ALL_ITEMS.map((item) => ({
    item,
    depth: Math.max(
      ...[item.href, ...(item.tabs ?? []).map((tab) => tab.href)].map((href) =>
        isAtOrBelow(path, href) ? href.length : -1
      )
    )
  })).filter(({ depth }) => depth >= 0);
  return scored.reduce<(typeof scored)[number] | undefined>(
    (best, entry) => (!best || entry.depth > best.depth ? entry : best),
    undefined
  )?.item;
}

export type Crumb = { readonly label: string; readonly href?: string };

export function breadcrumbs(path: string): readonly Crumb[] {
  const item = owningItem(path);
  if (!item || item === OVERVIEW) return [];
  const section = sectionOf(item);
  const tab = activeTab(path, item.tabs ?? []);
  const leafHref = tab?.href ?? item.tabs?.[0].href ?? item.href;
  const normalized = path === '/' ? '/' : path.replace(/\/+$/, '');
  const detailSuffix =
    normalized !== leafHref && normalized.startsWith(`${leafHref}/`)
      ? normalized.slice(leafHref.length + 1)
      : '';
  const lastSegment = detailSuffix.split('/').filter(Boolean).at(-1);
  const detailLabel =
    lastSegment === 'new'
      ? 'New'
      : lastSegment === 'revisions'
        ? 'Revisions'
        : lastSegment
          ? 'Details'
          : null;
  return [
    ...(section ? [{ label: section }] : []),
    { label: item.label, href: item.tabs?.[0].href ?? item.href },
    ...(tab && tab.label !== item.label
      ? [{ label: tab.label, href: tab.href }]
      : []),
    ...(detailLabel ? [{ label: detailLabel }] : [])
  ];
}

export function paletteEntries(
  grant: Grant,
  gatewayAvailable: boolean
): readonly PaletteEntry[] {
  const pinned = [OVERVIEW, PLAYGROUND, SETTINGS, PROFILE]
    .map((item) => visibleItem(item, grant, gatewayAvailable))
    .filter((item): item is VisibleItem => item !== null);
  const pages = [
    ...pinned.map((visible) => ({ section: 'Go to', visible })),
    ...visibleSections(grant, gatewayAvailable).flatMap((section) =>
      section.items.map((visible) => ({ section: section.label, visible }))
    )
  ].flatMap(({ section, visible }) => [
    {
      label: visible.item.label,
      href: visible.href,
      group: section,
      icon: visible.item.icon
    },
    ...visible.tabs
      .filter((tab) => tab.label !== visible.item.label)
      .map((tab) => ({
        label: `${visible.item.label} › ${tab.label}`,
        href: tab.href,
        group: section,
        icon: visible.item.icon
      }))
  ]);
  const actions = CREATE_ACTIONS.filter((action) =>
    leafVisible(action, grant, gatewayAvailable)
  ).map(({ label, href, group, icon }) => ({ label, href, group, icon }));
  return [...actions, ...pages];
}
