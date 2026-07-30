const FIXED_ROLE_VALUES = ['owner', 'operator', 'developer', 'viewer'] as const;
export type FixedRole = (typeof FIXED_ROLE_VALUES)[number];

const CAPABILITY_VALUES = [
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

export function can(
  role: FixedRole | null | undefined,
  capability: Capability
): boolean {
  return role ? ROLE_CAPABILITIES[role].has(capability) : false;
}
