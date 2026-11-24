import { resolve } from '$app/paths';

export type TabLink = {
  readonly label: string;
  readonly href: string;
};

export const isAtOrBelow = (path: string, ancestor: string) =>
  ancestor === resolve('')
    ? path === ancestor
    : path === ancestor || path.startsWith(`${ancestor}/`);

export function activeTab<Tab extends Pick<TabLink, 'href'>>(
  path: string,
  tabs: readonly Tab[]
): Tab | undefined {
  return tabs
    .filter((tab) => isAtOrBelow(path, tab.href))
    .reduce<Tab | undefined>(
      (best, tab) => (!best || tab.href.length > best.href.length ? tab : best),
      undefined
    );
}
