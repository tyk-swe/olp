import { describe, expect, it } from 'vitest';
import {
  FIXED_ROLES,
  can,
  isFixedRole,
  type Capability,
  type FixedRole
} from './authorization';

const CAPABILITY_INVENTORY = {
  'configuration.read': true,
  'providers.manage': true,
  'routes.manage': true,
  'api_keys.read': true,
  'api_keys.manage': true,
  'users.read': true,
  'users.manage': true,
  'sessions.manage': true,
  'operations.read': true,
  'playground.use': true,
  'settings.read': true,
  'settings.update': true,
  'pricing.update': true
} satisfies Record<Capability, true>;
const CAPABILITIES = Object.keys(CAPABILITY_INVENTORY) as Capability[];

const EXPECTED_CAPABILITIES: Record<FixedRole, readonly Capability[]> = {
  owner: CAPABILITIES,
  operator: [
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
  ],
  developer: [
    'configuration.read',
    'api_keys.read',
    'api_keys.manage',
    'operations.read',
    'playground.use',
    'settings.read'
  ],
  viewer: [
    'configuration.read',
    'api_keys.read',
    'operations.read',
    'settings.read'
  ]
};

describe('fixed-role authorization', () => {
  it('exposes the canonical fixed-role inventory', () => {
    expect(FIXED_ROLES).toEqual(['owner', 'operator', 'developer', 'viewer']);
  });

  it.each(
    Object.entries(EXPECTED_CAPABILITIES) as [
      FixedRole,
      readonly Capability[]
    ][]
  )('%s has exactly its declared capabilities', (role, expected) => {
    const granted = CAPABILITIES.filter((capability) => can(role, capability));
    expect(granted).toEqual(expected);
  });

  it.each([
    ['owner', true],
    ['operator', true],
    ['developer', true],
    ['viewer', true],
    ['Owner', false],
    ['administrator', false],
    ['', false],
    [null, false],
    [42, false]
  ])('validates the closed role value %j', (value, expected) => {
    expect(isFixedRole(value)).toBe(expected);
  });

  it('denies capabilities when no principal role is available', () => {
    expect(can(null, 'configuration.read')).toBe(false);
    expect(can(undefined, 'configuration.read')).toBe(false);
  });
});
