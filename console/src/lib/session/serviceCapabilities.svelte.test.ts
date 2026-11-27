import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountState } from '#test/mount.js';
import { useServiceCapabilities } from './serviceCapabilities.svelte';

function capabilities() {
  return mountState(() => useServiceCapabilities()).value;
}

describe('service capabilities', () => {
  it('keeps capabilities unavailable until the server responds', async () => {
    const response = Promise.withResolvers<Response>();
    serve((request) => {
      route(request, 'GET', '/api/v1/auth/capabilities');
      return response.promise;
    });
    const state = capabilities();

    expect(state.pending).toBe(true);
    expect(state.localLoginEnabled).toBe(false);
    expect(state.gatewayAvailable).toBe(false);
    expect(state.limitsEnforced).toBe(false);
    expect(state.retentionEnforced).toBe(false);
    response.resolve(
      json({ local_login_enabled: true, oidc_login_enabled: false })
    );
    await eventually(() => expect(state.pending).toBe(false));

    expect(state.localLoginEnabled).toBe(true);
    expect(state.gatewayAvailable).toBe(true);
    expect(state.limitsEnforced).toBe(true);
    expect(state.retentionEnforced).toBe(true);
    expect(state.managementNetworkRestricted).toBe(false);
    expect(state.notificationsActive).toBe(false);
    expect(state.payloadCaptureActive).toBe(false);
    expect(state.error).toBeNull();
  });

  it('reflects explicitly enabled and disabled server capabilities after refresh', async () => {
    const response = vi
      .fn(() => json({ local_login_enabled: false, oidc_login_enabled: true }))
      .mockReturnValueOnce(
        json({
          local_login_enabled: true,
          oidc_login_enabled: false,
          gateway_available: false,
          limits_enforced: false,
          retention_enforced: false,
          management_network_restricted: true,
          notifications_active: true,
          payload_capture_active: true
        })
      );
    serve(response);
    const state = capabilities();
    await eventually(() => expect(state.pending).toBe(false));

    expect(state.localLoginEnabled).toBe(true);
    expect(state.gatewayAvailable).toBe(false);
    expect(state.limitsEnforced).toBe(false);
    expect(state.retentionEnforced).toBe(false);
    expect(state.managementNetworkRestricted).toBe(true);
    expect(state.notificationsActive).toBe(true);
    expect(state.payloadCaptureActive).toBe(true);
    await state.retry();
    await eventually(() => expect(state.localLoginEnabled).toBe(false));

    expect(state.gatewayAvailable).toBe(true);
    expect(state.limitsEnforced).toBe(true);
    expect(state.retentionEnforced).toBe(true);
    expect(state.managementNetworkRestricted).toBe(false);
    expect(state.notificationsActive).toBe(false);
    expect(state.payloadCaptureActive).toBe(false);
  });
});
