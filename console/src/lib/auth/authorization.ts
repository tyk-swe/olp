import { getContext, setContext } from 'svelte';

export const FIXED_ROLE_VALUES = ['owner', 'operator', 'developer', 'viewer'] as const;
export type FixedRole = (typeof FIXED_ROLE_VALUES)[number];

export const CAPABILITY_VALUES = [
  'configuration.read',
  'providers.manage',
  'routes.manage',
  'api_keys.read',
  'api_keys.manage',
  'users.read',
  'users.manage',
  'sessions.manage',
  'operations.read',
  'playground.use',
  'settings.read',
  'settings.update',
  'pricing.update'
] as const;
export type Capability = (typeof CAPABILITY_VALUES)[number];

const FIXED_ROLES = new Set<string>(FIXED_ROLE_VALUES);
const ALL_CAPABILITIES = new Set<Capability>(CAPABILITY_VALUES);
const ROLE_CAPABILITIES: Record<FixedRole, ReadonlySet<Capability>> = {
  owner: ALL_CAPABILITIES,
  operator: new Set([
    'configuration.read',
    'providers.manage',
    'routes.manage',
    'api_keys.read',
    'api_keys.manage',
    'users.read',
    'operations.read',
    'playground.use',
    'settings.read',
    'settings.update',
    'pricing.update'
  ]),
  developer: new Set([
    'configuration.read',
    'api_keys.read',
    'api_keys.manage',
    'operations.read',
    'playground.use',
    'settings.read'
  ]),
  viewer: new Set([
    'configuration.read',
    'api_keys.read',
    'operations.read',
    'settings.read'
  ])
};

export function isFixedRole(value: unknown): value is FixedRole {
  return typeof value === 'string' && FIXED_ROLES.has(value);
}

export function capabilitiesForRole(role: FixedRole | null | undefined): ReadonlySet<Capability> {
  return role ? ROLE_CAPABILITIES[role] : new Set<Capability>();
}

export function roleHasCapability(
  role: FixedRole | null | undefined,
  capability: Capability
): boolean {
  return capabilitiesForRole(role).has(capability);
}

export type Authorization = {
  role(): FixedRole | null;
  capabilities(): ReadonlySet<Capability>;
  can(capability: Capability): boolean;
};

const AUTHORIZATION_CONTEXT = Symbol('olp-authorization');
const DENY_ALL: Authorization = {
  role: () => null,
  capabilities: () => new Set<Capability>(),
  can: () => false
};

export function provideAuthorization(getRole: () => FixedRole | null): Authorization {
  const authorization: Authorization = {
    role: getRole,
    capabilities: () => capabilitiesForRole(getRole()),
    can: (capability) => roleHasCapability(getRole(), capability)
  };
  setContext(AUTHORIZATION_CONTEXT, authorization);
  return authorization;
}

export function useAuthorization(): Authorization {
  return getContext<Authorization | undefined>(AUTHORIZATION_CONTEXT) ?? DENY_ALL;
}
