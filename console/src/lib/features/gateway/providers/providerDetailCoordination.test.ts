import { describe, expect, it } from 'vitest';
import { providerModelPageKey } from './providerDetailCoordination';

describe('providerModelPageKey', () => {
  it('pins cached model pages to the provider snapshot etag', () => {
    expect(
      providerModelPageKey(
        'fallback-id',
        { id: 'provider-id', etag: 'etag-7' } as never,
        'next-page'
      )
    ).toEqual(['provider-model-page', 'provider-id', 'next-page', 'etag-7']);
  });

  it('provides stable placeholders before the provider is loaded', () => {
    expect(providerModelPageKey('provider-id', undefined, undefined)).toEqual([
      'provider-model-page',
      'provider-id',
      'first',
      'unversioned'
    ]);
  });
});
