import { resolve } from '$app/paths';
import type { TabLink } from '#lib/components/tabLinks.js';

export const routeTabs = (routeId: string): readonly TabLink[] => [
  { label: 'Draft', href: resolve(`routes/${routeId}`) },
  { label: 'Revision history', href: resolve(`routes/${routeId}/revisions`) }
];
