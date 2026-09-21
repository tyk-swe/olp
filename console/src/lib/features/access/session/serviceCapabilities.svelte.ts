import { createQuery } from '@tanstack/svelte-query';
import { authenticationCapabilities } from './auth';

export function useServiceCapabilities() {
  const capabilities = createQuery(() => ({
    queryKey: ['service-capabilities'],
    queryFn: ({ signal }) => authenticationCapabilities(signal),
    staleTime: 60_000
  }));
  return {
    get localLoginEnabled() {
      return capabilities.isSuccess && capabilities.data.local_login_enabled;
    },
    get gatewayAvailable() {
      return (
        capabilities.isSuccess &&
        (!('gateway_available' in capabilities.data) ||
          capabilities.data.gateway_available !== false)
      );
    },
    get limitsEnforced() {
      return (
        capabilities.isSuccess &&
        (!('limits_enforced' in capabilities.data) ||
          capabilities.data.limits_enforced !== false)
      );
    },
    get notificationsActive() {
      return (
        capabilities.isSuccess &&
        capabilities.data.notifications_active === true
      );
    },
    get retentionEnforced() {
      return (
        capabilities.isSuccess &&
        (!('retention_enforced' in capabilities.data) ||
          capabilities.data.retention_enforced !== false)
      );
    },
    get pending() {
      return capabilities.isPending;
    },
    get error() {
      return capabilities.error;
    },
    retry() {
      return capabilities.refetch();
    }
  };
}
