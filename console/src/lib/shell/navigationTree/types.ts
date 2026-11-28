import type { TabLink } from '#lib/components/tabLinks.js';
import type { ManagementRoute } from '#lib/management/requirements.js';
import type { IconName } from '#lib/components/icons.js';

export type NavigationLeaf = TabLink & {
  readonly route?: ManagementRoute;
  readonly controlPlane?: true;
};

export type NavigationItem = NavigationLeaf & {
  readonly icon: IconName;
  readonly description?: string;
  readonly tabs?: readonly NavigationLeaf[];
};

export type NavigationSection = {
  readonly label: string;
  readonly items: readonly NavigationItem[];
};
