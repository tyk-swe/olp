import { validated, type Validation } from '#lib/editing/validation.js';
import type { Mapping } from './api';

export type Membership = {
  id: string;
  value: string;
  role: NonNullable<Mapping['projects']>[number]['role'];
};
export type MappingDraft = {
  role: NonNullable<Mapping['role']> | '';
  scope: NonNullable<Mapping['accessScope']>;
  memberships: Membership[];
};

function projectError(member: Membership, index: number, all: Membership[]) {
  if (!member.value) return 'Choose a project.';
  return all.findIndex((other) => other.value === member.value) === index
    ? undefined
    : 'Choose each project at most once.';
}

export function validateMapping({
  role,
  scope,
  memberships
}: MappingDraft): Validation<Mapping, string> {
  return validated<Mapping, string>(
    Object.fromEntries(
      memberships.map((member, index) => [
        member.id,
        projectError(member, index, memberships)
      ])
    ),
    () => ({
      ...(role ? { role } : {}),
      accessScope: scope,
      projects: memberships.map(({ value, role }) => ({ value, role }))
    })
  );
}
