import { describe, expect, it } from 'vitest';
import {
  bundledCatalogHint,
  catalogProvenanceLabel,
  sourceAddress,
  sourceFormatLabel
} from './pricingSourcePresentation';

describe('pricing source presentation', () => {
  it('names a catalog source without a URL as the bundled catalog', () => {
    expect(sourceFormatLabel({ format: 'catalog' })).toBe(
      'Signed reference catalog'
    );
    expect(sourceFormatLabel({ format: 'prices' })).toBe('Price list');
    expect(sourceAddress({ url: null })).toBe(
      'Catalog bundled with this release'
    );
    expect(sourceAddress({ url: 'https://example.test/c.json' })).toBe(
      'https://example.test/c.json'
    );
  });

  it('labels the signed catalog a snapshot came from', () => {
    expect(catalogProvenanceLabel({ catalog: null })).toBe('');
    expect(
      catalogProvenanceLabel({
        catalog: {
          sha256: 'abcdef0123456789',
          published_at: '2026-10-05T12:00:00Z',
          key_id: 'release-2026a'
        }
      })
    ).toMatch(/signed by release-2026a · abcdef012345…$/);
  });

  it('describes the bundled catalog when it is known', () => {
    expect(
      bundledCatalogHint({
        api_version: 'openllmproxy.dev/catalog/v1',
        published_at: '2026-10-05T12:00:00Z',
        sha256: 'a',
        key_id: 'k',
        vendor_count: 14,
        model_count: 334
      })
    ).toContain('334 models from 14 vendors');
    expect(bundledCatalogHint(undefined)).toContain(
      'bundled with this release'
    );
  });
});
