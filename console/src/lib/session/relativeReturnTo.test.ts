import { describe, expect, it } from 'vitest';
import { relativeReturnTo } from './relativeReturnTo';

const origin = 'https://console.example.test';

describe('post-login return paths', () => {
  it.each([
    '/',
    '/routes',
    '/routes/example?tab=policy&view=compact#rules',
    '/path%20with%20spaces?name=one%20two',
    '/routes/../usage'
  ])('accepts the local path %s', (path) => {
    const result = relativeReturnTo(path, origin);
    const parsed = new URL(path, origin);

    expect(new URL(result, origin).origin).toBe(origin);
    expect(result).toBe(`${parsed.pathname}${parsed.search}${parsed.hash}`);
  });

  it.each([
    '',
    'routes',
    'https://outside.example.test/routes',
    '//outside.example.test/routes',
    '/\\outside.example.test',
    '/route\nlocation',
    '/route%0Alocation',
    '/route%5Clocation',
    '/%2Foutside.example.test',
    '/broken%',
    '/broken%2',
    '/broken%xy',
    '/%C0%AF',
    '/login',
    '/login/step',
    '/%6cogin',
    '/routes/../login',
    '/api/v1/oidc/login',
    '/api/v1/oidc/callback/complete'
  ])(
    'falls back to the console root for unsafe or looping input %s',
    (path) => {
      expect(relativeReturnTo(path, origin)).toBe('/');
    }
  );

  it('limits the byte length of both submitted and canonical paths', () => {
    const allowed = `/${'a'.repeat(2047)}`;

    expect(relativeReturnTo(allowed, origin)).toBe(allowed);
    expect(relativeReturnTo(`${allowed}a`, origin)).toBe('/');
    expect(relativeReturnTo(`/${'é'.repeat(1000)}`, origin)).toBe('/');
  });

  it('falls back when the return path or origin is missing or invalid', () => {
    expect(relativeReturnTo(null, origin)).toBe('/');
    expect(relativeReturnTo(undefined, origin)).toBe('/');
    expect(relativeReturnTo('/routes', 'not-an-origin')).toBe('/');
  });
});
