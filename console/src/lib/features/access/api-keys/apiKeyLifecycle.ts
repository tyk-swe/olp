import type { ApiKey } from '#lib/domain/api-keys/api.js';

export function isApiKeyExpired(
  key: Pick<ApiKey, 'expires_at'>,
  now = Date.now()
): boolean {
  return Boolean(key.expires_at && new Date(key.expires_at).getTime() < now);
}

export function isApiKeyUsable(
  key: Pick<ApiKey, 'expires_at' | 'revoked_at'>,
  now = Date.now()
): boolean {
  return (
    !key.revoked_at &&
    (!key.expires_at || new Date(key.expires_at).getTime() >= now)
  );
}
