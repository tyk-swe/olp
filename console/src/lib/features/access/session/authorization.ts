import {
  MANAGEMENT_REQUIREMENTS,
  type ManagementOperation,
  type ManagementRoute
} from '$lib/api/requirements';

export const FIXED_ROLES = [
  'owner',
  'operator',
  'developer',
  'viewer'
] as const;
export type FixedRole = (typeof FIXED_ROLES)[number];

const FIXED_ROLE_SET = new Set<string>(FIXED_ROLES);

export function isFixedRole(value: unknown): value is FixedRole {
  return typeof value === 'string' && FIXED_ROLE_SET.has(value);
}

/**
 * What a signed-in member holds, exactly as the server reported it: the
 * management operations its policy grants the member now, and whether the
 * member's access reaches the whole installation.
 */
export type Grant = {
  readonly operations: readonly ManagementOperation[];
  readonly access_scope: 'global' | 'assigned';
};

/**
 * Whether the server would admit the member's call to a management route,
 * evaluated against the security requirement the contract declares for it.
 * Handlers may still refuse a call they admit, for example for a project the
 * member cannot change.
 */
export function allows(
  grant: Grant | null | undefined,
  route: ManagementRoute
): boolean {
  if (!grant) return false;
  const requirement = MANAGEMENT_REQUIREMENTS[route];
  if (requirement.public) return true;
  return requirement.alternatives.some(
    (alternative) =>
      alternative.kind === 'user' &&
      alternative.operations.every((operation) =>
        grant.operations.includes(operation)
      ) &&
      (!alternative.installation || grant.access_scope === 'global')
  );
}

/** Whether the member holds a management operation. */
export function holds(
  grant: Grant | null | undefined,
  operation: ManagementOperation
): boolean {
  return grant?.operations.includes(operation) ?? false;
}

/**
 * Each console capability is the management call it leads to, so the console
 * offers an action only when the server would admit that call.
 */
const CAPABILITY_ROUTES = {
  'configuration.read': 'GET /api/v1/providers',
  'providers.manage': 'POST /api/v1/providers',
  'routes.manage': 'POST /api/v1/route-drafts',
  'api_keys.read': 'GET /api/v1/api-keys',
  'api_keys.manage': 'POST /api/v1/api-keys',
  'users.read': 'GET /api/v1/users',
  'users.manage': 'PATCH /api/v1/users/{user_id}',
  'operations.read': 'GET /api/v1/requests',
  'media.manage': 'DELETE /api/v1/media-jobs/{job_id}',
  'playground.use': 'POST /api/v1/playground',
  'settings.read': 'GET /api/v1/settings',
  'settings.update': 'PUT /api/v1/settings/{key}',
  'pricing.update': 'POST /api/v1/pricing/revisions'
} as const satisfies Record<string, ManagementRoute>;

export type Capability = keyof typeof CAPABILITY_ROUTES | 'sessions.manage';

export function can(
  grant: Grant | null | undefined,
  capability: Capability
): boolean {
  // Managing other members' sessions refines the member's own session routes.
  if (capability === 'sessions.manage') return holds(grant, 'manage_sessions');
  return allows(grant, CAPABILITY_ROUTES[capability]);
}
