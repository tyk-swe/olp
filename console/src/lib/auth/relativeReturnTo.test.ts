import { describe, expect, it } from 'vitest';
import { relativeReturnTo } from './relativeReturnTo';

const ORIGIN = 'https://console.example.test';

describe('relativeReturnTo', () => {
  it.each([
    ['/settings', '/settings'],
    ['/settings?tab=security', '/settings?tab=security'],
    ['/files/a%20b', '/files/a%20b'],
    ['/search?q=hello%20world&next=%2Fmodels', '/search?q=hello%20world&next=%2Fmodels'],
    ['/settings?tab=security#sessions', '/settings?tab=security#sessions'],
    ['/a/../settings', '/settings']
  ])('accepts and canonicalizes %s', (input, expected) => {
    expect(relativeReturnTo(input, ORIGIN)).toBe(expected);
  });

  it.each([
    'https://attacker.example/',
    '//attacker.example/',
    '/\\attacker.example/',
    '/%5c%5cattacker.example/',
    '/%2fattacker.example/',
    '/a/..//attacker.example/',
    '/%2e%2e//attacker.example/',
    '/%2e%2e%2f%2fattacker.example/',
    '/safe%2f..%2flogin',
    '/deep%2f..%2f..%2flogin',
    '/bad%encoding',
    '/control\u0000',
    '/encoded%C2%85control',
    '/invalid-utf8-%ff',
    '/safe%0d%0aheader',
    '/login',
    '/login?return_to=%2Fsettings',
    '/a/../login',
    '/%6cogin',
    '/api/v1/oidc/callback'
  ])('rejects %s', (input) => {
    expect(relativeReturnTo(input, ORIGIN)).toBe('/');
  });

  it('rejects destinations that exceed the cookie-safe bound', () => {
    expect(relativeReturnTo(`/${'a'.repeat(2_048)}`, ORIGIN)).toBe('/');
  });
});
