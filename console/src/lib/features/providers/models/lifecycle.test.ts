import { describe, expect, it } from 'vitest';
import { lifecycleNotice } from './lifecycle';

const today = new Date('2026-10-05T15:00:00Z');
const source = 'https://example.test/deprecations';

describe('lifecycleNotice', () => {
  it('says nothing without a documented lifecycle or a distant retirement', () => {
    expect(lifecycleNotice(null, today)).toBeNull();
    expect(
      lifecycleNotice(
        {
          deprecated_at: null,
          retires_at: '2027-06-01',
          replacement: null,
          source
        },
        today
      )
    ).toBeNull();
  });

  it('warns about a retirement within ninety days and names the replacement', () => {
    expect(
      lifecycleNotice(
        {
          deprecated_at: '2026-09-30',
          retires_at: '2026-11-30',
          replacement: 'claude-sonnet-5-5',
          source
        },
        today
      )
    ).toEqual({
      tone: 'warning',
      text: 'Retires on 2026-11-30, in 56 days. Its vendor names claude-sonnet-5-5 as the replacement.'
    });
  });

  it('warns about a deprecated model whose retirement is not yet near', () => {
    expect(
      lifecycleNotice(
        {
          deprecated_at: '2026-09-01',
          retires_at: '2027-09-01',
          replacement: null,
          source
        },
        today
      )?.text
    ).toBe('Deprecated by its vendor on 2026-09-01; retires on 2027-09-01.');
  });

  it('marks a retired model as danger', () => {
    expect(
      lifecycleNotice(
        {
          deprecated_at: null,
          retires_at: '2026-10-05',
          replacement: null,
          source
        },
        today
      )
    ).toEqual({ tone: 'danger', text: 'Retired by its vendor on 2026-10-05.' });
  });
});
