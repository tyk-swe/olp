import type { PluginIndex, PluginIndexEntry, PluginIndexRelease } from './api';

/** Where a reviewed release stands in this installation. */
export function releaseStatus(release: PluginIndexRelease): string {
  if (release.approved) return 'Approved';
  if (release.installed) return 'Installed, awaiting approval';
  return 'Not installed';
}

/** The reviewed plugin and release an installed digest is, if any. */
export function indexEntryForDigest(
  index: PluginIndex | undefined,
  digest: string
): { plugin: PluginIndexEntry; release: PluginIndexRelease } | null {
  for (const plugin of index?.items ?? []) {
    const release = plugin.releases.find(
      (candidate) => candidate.digest === digest
    );
    if (release) return { plugin, release };
  }
  return null;
}

/** The source of a release at the commit its digest builds from. */
export function sourceURL(
  plugin: Pick<PluginIndexEntry, 'repository' | 'path'>,
  release: Pick<PluginIndexRelease, 'commit'>
): string {
  return `${plugin.repository}/tree/${release.commit}/${plugin.path}`;
}
