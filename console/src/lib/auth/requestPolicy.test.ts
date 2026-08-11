import { describe, expect, it } from 'vitest';
import {
  combineSignals,
  isAuthenticationEndpoint,
  isCurrentSessionDeletion,
  isMutationRequest,
  isSessionValidationEndpoint
} from './requestPolicy';

const request = (method: string, pathname: string): Request =>
  new Request(`https://console.example.test${pathname}`, { method });

describe('authentication request policy', () => {
  it.each([
    ['GET', '/api/v1/setup/status'],
    ['POST', '/api/v1/setup'],
    ['POST', '/api/v1/sessions'],
    ['POST', '/api/v1/invitations/accept'],
    ['GET', '/api/v1/auth/capabilities'],
    ['GET', '/api/v1/oidc/login'],
    ['POST', '/api/v1/oidc/login'],
    ['GET', '/api/v1/oidc/callback?code=one&state=two']
  ])('recognizes the exact public route %s %s', (method, pathname) => {
    expect(isAuthenticationEndpoint(request(method, pathname))).toBe(true);
  });

  it.each([
    ['GET', '/api/v1/setup'],
    ['HEAD', '/api/v1/setup/status'],
    ['OPTIONS', '/api/v1/sessions'],
    ['POST', '/api/v1/sessions/'],
    ['POST', '/api/v1/sessions/nested'],
    ['POST', '/api/v1/oidc/link'],
    ['GET', '/api/v1/oidc/callback/extra']
  ])('rejects a widened public route %s %s', (method, pathname) => {
    expect(isAuthenticationEndpoint(request(method, pathname))).toBe(false);
  });

  it('distinguishes current-session reads and deletion from nearby requests', () => {
    expect(
      isSessionValidationEndpoint(
        request('GET', '/api/v1/sessions/current?fresh=true')
      )
    ).toBe(true);
    expect(
      isSessionValidationEndpoint(request('POST', '/api/v1/sessions/current'))
    ).toBe(false);
    expect(
      isCurrentSessionDeletion(request('DELETE', '/api/v1/sessions/current'))
    ).toBe(true);
    expect(
      isCurrentSessionDeletion(request('DELETE', '/api/v1/sessions/current/'))
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
    expect(isMutationRequest(request(method, '/api/v1/profile'))).toBe(
      expected
    );
  });
});

describe('combined cancellation signals', () => {
  it('propagates a source cancellation reason', () => {
    const first = new AbortController();
    const second = new AbortController();
    const combined = combineSignals(first.signal, second.signal);

    second.abort('second request ended');

    expect(combined.aborted).toBe(true);
    expect(combined.reason).toBe('second request ended');
  });

  it('preserves cancellation semantics without AbortSignal.any', () => {
    const descriptor = Object.getOwnPropertyDescriptor(AbortSignal, 'any');
    Object.defineProperty(AbortSignal, 'any', {
      configurable: true,
      value: undefined
    });

    try {
      const alreadyAborted = new AbortController();
      const pending = new AbortController();
      alreadyAborted.abort('already cancelled');
      const combined = combineSignals(alreadyAborted.signal, pending.signal);

      expect(combined.aborted).toBe(true);
      expect(combined.reason).toBe('already cancelled');

      const left = new AbortController();
      const right = new AbortController();
      const later = combineSignals(left.signal, right.signal);
      right.abort('cancelled later');
      expect(later.reason).toBe('cancelled later');
    } finally {
      if (descriptor) {
        Object.defineProperty(AbortSignal, 'any', descriptor);
      } else {
        Reflect.deleteProperty(AbortSignal, 'any');
      }
    }
  });
});
