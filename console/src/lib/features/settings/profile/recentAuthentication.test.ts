import { describe, expect, it } from 'vitest';
import { parseRecentAuthenticationCallback } from './recentAuthentication';

describe('recent-authentication callback parsing', () => {
  it('accepts only the three closed purposes', () => {
    expect(
      parseRecentAuthenticationCallback('?reauthenticated=oidc_link')
    ).toEqual({ purpose: 'oidc_link', resourceId: undefined });
    expect(
      parseRecentAuthenticationCallback(
        '?reauthenticated=oidc_unlink&resource_id=identity-1'
      )
    ).toEqual({ purpose: 'oidc_unlink', resourceId: 'identity-1' });
    expect(
      parseRecentAuthenticationCallback('?reauthenticated=unexpected')
    ).toBeNull();
  });
});
