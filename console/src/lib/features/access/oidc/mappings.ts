import { FIXED_ROLES } from '../shared';

export function parseRoleMappings(value: string) {
  return value
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => {
      const separator = line.lastIndexOf('=');
      if (separator < 1)
        throw new Error(`Mapping “${line}” must use claim-value=role.`);
      const claim_value = line.slice(0, separator).trim();
      const role = line.slice(separator + 1).trim();
      if (!FIXED_ROLES.some((fixedRole) => fixedRole === role)) {
        throw new Error(`Mapping “${line}” has an invalid fixed role.`);
      }
      return { claim_value, role };
    });
}
