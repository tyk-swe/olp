import { describe, expect, it } from 'vitest';
import type { PluginIndex, PluginIndexRelease } from './api';
import { indexEntryForDigest, releaseStatus, sourceURL } from './pluginIndex';

const release: PluginIndexRelease = {
  version: '0.1.0',
  digest: 'a'.repeat(64),
  abi_version: 1,
  size_bytes: 1024,
  origins: ['https://api.example.test'],
  profiles: ['example-chat'],
  commit: 'b'.repeat(40),
  reviewed_at: '2026-10-05T00:00:00Z',
  installed: false,
  approved: false
};
const index: PluginIndex = {
  published_at: '2026-10-05T00:00:00Z',
  sha256: 'c'.repeat(64),
  key_id: 'dev-2026a',
  items: [
    {
      name: 'example',
      description: 'Example.',
      maintainer: 'Example',
      documentation_url: 'https://example.test/docs',
      repository: 'https://github.com/example/plugins',
      path: 'plugins/example',
      releases: [release]
    }
  ]
};

describe('plugin index', () => {
  it('reports where a reviewed release stands', () => {
    expect(releaseStatus(release)).toBe('Not installed');
    expect(releaseStatus({ ...release, installed: true })).toBe(
      'Installed, awaiting approval'
    );
    expect(releaseStatus({ ...release, installed: true, approved: true })).toBe(
      'Approved'
    );
  });

  it('finds the reviewed release of an installed digest', () => {
    expect(indexEntryForDigest(index, 'a'.repeat(64))?.plugin.name).toBe(
      'example'
    );
    expect(indexEntryForDigest(index, 'd'.repeat(64))).toBeNull();
    expect(indexEntryForDigest(undefined, 'a'.repeat(64))).toBeNull();
  });

  it('links a release to its source at the reviewed commit', () => {
    expect(sourceURL(index.items[0]!, release)).toBe(
      `https://github.com/example/plugins/tree/${'b'.repeat(40)}/plugins/example`
    );
  });
});
