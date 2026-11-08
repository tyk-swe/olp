import { resolve } from '$app/paths';
import type { NavigationLeaf } from '#lib/shell/navigation.js';

export const routeTabs = (routeId: string): readonly NavigationLeaf[] => [
  { label: 'Draft', href: resolve(`routes/${routeId}`) },
  { label: 'Revision history', href: resolve(`routes/${routeId}/revisions`) }
];
