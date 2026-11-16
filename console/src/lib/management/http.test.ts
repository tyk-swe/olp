import { describe, expect, it } from 'vitest';
import { mutationHeaders, serializeIfMatch } from './http';

describe('serializeIfMatch', () => {
  it('quotes a bare UUID etag', () => {
    expect(serializeIfMatch('018f2b6a-9f53-7c21-8c3d-2f6b8f0e5a77')).toBe(
      '"018f2b6a-9f53-7c21-8c3d-2f6b8f0e5a77"'
    );
  });

  it('quotes a bare non-UUID etag', () => {
    expect(serializeIfMatch('rev-42')).toBe('"rev-42"');
  });

  it('leaves quoted, weak and wildcard values alone', () => {
    expect(serializeIfMatch('"rev-42"')).toBe('"rev-42"');
    expect(serializeIfMatch('W/"rev-42"')).toBe('W/"rev-42"');
    expect(serializeIfMatch('*')).toBe('*');
  });
});

describe('mutation headers', () => {
  it('pairs the quoted etag with the request identity', () => {
    expect(mutationHeaders('etag-1', 'req-1')).toEqual({
      'If-Match': '"etag-1"',
      'Idempotency-Key': 'req-1'
    });
  });
});
