import { afterEach, expect, it, vi } from 'vitest';
import { isApiKeyExpired, isApiKeyUsable } from './apiKeyLifecycle';

const expiry = '2026-07-12T12:00:00Z';
const now = new Date(expiry).getTime();

afterEach(() => {
  vi.restoreAllMocks();
});

it.each([
  {
    name: 'no expiry',
    expiresAt: null,
    clock: now,
    expired: false,
    usable: true
  },
  {
    name: 'empty expiry',
    expiresAt: '',
    clock: now,
    expired: false,
    usable: true
  },
  {
    name: 'future expiry',
    expiresAt: expiry,
    clock: now - 1,
    expired: false,
    usable: true
  },
  {
    name: 'equal expiry',
    expiresAt: expiry,
    clock: now,
    expired: false,
    usable: true
  },
  {
    name: 'past expiry',
    expiresAt: expiry,
    clock: now + 1,
    expired: true,
    usable: false
  },
  {
    name: 'invalid expiry',
    expiresAt: 'not-a-date',
    clock: now,
    expired: false,
    usable: false
  }
])(
  'preserves lifecycle comparisons for $name',
  ({ expiresAt, clock, expired, usable }) => {
    const key = { expires_at: expiresAt, revoked_at: null };

    expect(isApiKeyExpired(key, clock)).toBe(expired);
    expect(isApiKeyUsable(key, clock)).toBe(usable);
  }
);

it.each([null, expiry, 'not-a-date'])(
  'keeps a revoked key unusable regardless of its expiry: %s',
  (expiresAt) => {
    expect(
      isApiKeyUsable({ expires_at: expiresAt, revoked_at: expiry }, now)
    ).toBe(false);
  }
);

it('still identifies an expired key after revocation', () => {
  const key = { expires_at: expiry, revoked_at: expiry };

  expect(isApiKeyExpired(key, now + 1)).toBe(true);
  expect(isApiKeyUsable(key, now + 1)).toBe(false);
});

it('uses the current clock when a caller does not supply one', () => {
  vi.spyOn(Date, 'now').mockReturnValue(now + 1);
  const key = { expires_at: expiry, revoked_at: null };

  expect(isApiKeyExpired(key)).toBe(true);
  expect(isApiKeyUsable(key)).toBe(false);
});
