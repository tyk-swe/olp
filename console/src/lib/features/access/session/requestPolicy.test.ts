import { describe, expect, it } from 'vitest';
import {
  isAuthenticationEndpoint,
  isCurrentSessionDeletion,
  isMutationRequest,
  isSessionValidationEndpoint
} from '$lib/features/access/session/requestPolicy';

const request = (method: string, pathname: string): Request =>
  new Request(`https://console.example.test${pathname}`, { method });

describe('authentication request policy', () => {
  it.each([
    ['GET', '/api/v3/setup/status'],
    ['POST', '/api/v3/setup'],
    ['POST', '/api/v3/sessions'],
    ['POST', '/api/v3/invitations/accept'],
    ['GET', '/api/v3/auth/capabilities'],
    ['GET', '/api/v3/oidc/login'],
    ['POST', '/api/v3/oidc/login'],
    ['GET', '/api/v3/oidc/callback?code=one&state=two']
  ])('recognizes the exact public route %s %s', (method, pathname) => {
    expect(isAuthenticationEndpoint(request(method, pathname))).toBe(true);
  });

  it.each([
    ['GET', '/api/v3/setup'],
    ['HEAD', '/api/v3/setup/status'],
    ['OPTIONS', '/api/v3/sessions'],
    ['POST', '/api/v3/sessions/'],
    ['POST', '/api/v3/sessions/nested'],
    ['POST', '/api/v3/oidc/link'],
    ['GET', '/api/v3/oidc/callback/extra']
  ])('rejects a widened public route %s %s', (method, pathname) => {
    expect(isAuthenticationEndpoint(request(method, pathname))).toBe(false);
  });

  it('distinguishes current-session reads and deletion from nearby requests', () => {
    expect(
      isSessionValidationEndpoint(
        request('GET', '/api/v3/sessions/current?fresh=true')
      )
    ).toBe(true);
    expect(
      isSessionValidationEndpoint(request('POST', '/api/v3/sessions/current'))
    ).toBe(false);
    expect(
      isCurrentSessionDeletion(request('DELETE', '/api/v3/sessions/current'))
    ).toBe(true);
    expect(
      isCurrentSessionDeletion(request('DELETE', '/api/v3/sessions/current/'))
    ).toBe(false);
  });

  it.each([
    ['GET', false],
    ['HEAD', false],
    ['OPTIONS', false],
    ['POST', true],
    ['PATCH', true],
    ['PUT', true],
    ['DELETE', true]
  ])('classifies %s mutation semantics', (method, expected) => {
    expect(isMutationRequest(request(method, '/api/v3/profile'))).toBe(
      expected
    );
  });
});
