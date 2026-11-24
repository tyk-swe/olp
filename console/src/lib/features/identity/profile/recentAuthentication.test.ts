import { expect, it } from 'vitest';
import { parseRecentAuthenticationCallback } from './recentAuthentication';

it('continues profile actions while leaving agent consent callbacks to their own page', () => {
  expect(
    parseRecentAuthenticationCallback(
      '?reauthenticated=oidc_unlink&resource_id=identity'
    )
  ).toEqual({
    purpose: 'oidc_unlink',
    resourceId: 'identity'
  });
  expect(
    parseRecentAuthenticationCallback('?reauthenticated=password_enrollment')
  ).toEqual({
    purpose: 'password_enrollment',
    resourceId: undefined
  });
  expect(
    parseRecentAuthenticationCallback(
      '?reauthenticated=agent_delegation&resource_id=key'
    )
  ).toBeNull();
  expect(
    parseRecentAuthenticationCallback('?reauthenticated=unexpected')
  ).toBeNull();
});
