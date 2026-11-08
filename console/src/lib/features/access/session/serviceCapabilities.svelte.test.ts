import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { json, route, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import { useServiceCapabilities } from './serviceCapabilities.svelte';

function capabilities() {
  const ready = vi.fn(
    (state: ReturnType<typeof useServiceCapabilities>) => state
  );
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: { setup: () => ready(useServiceCapabilities()) }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  return ready.mock.results[0].value;
}

afterEach(() => vi.unstubAllGlobals());

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
    await vi.waitFor(() => expect(state.pending).toBe(false));

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
    await vi.waitFor(() => expect(state.pending).toBe(false));

    expect(state.localLoginEnabled).toBe(true);
    expect(state.gatewayAvailable).toBe(false);
    expect(state.limitsEnforced).toBe(false);
    expect(state.retentionEnforced).toBe(false);
    expect(state.managementNetworkRestricted).toBe(true);
    expect(state.notificationsActive).toBe(true);
    expect(state.payloadCaptureActive).toBe(true);
    await state.retry();
    await vi.waitFor(() => expect(state.localLoginEnabled).toBe(false));

    expect(state.gatewayAvailable).toBe(true);
    expect(state.limitsEnforced).toBe(true);
    expect(state.retentionEnforced).toBe(true);
    expect(state.managementNetworkRestricted).toBe(false);
    expect(state.notificationsActive).toBe(false);
    expect(state.payloadCaptureActive).toBe(false);
  });
});
