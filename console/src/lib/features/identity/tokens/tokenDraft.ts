import { validated, type Validation } from '#lib/editing/validation.js';
import type { ManagementTokenScope } from './api';

export type TokenDraft = {
  name: string;
  scopes: ManagementTokenScope[];
  expiresInDays: number;
  allProjects: boolean;
  projectIds: string[];
};
export type TokenField = 'name' | 'scopes' | 'projectIds' | 'expiresInDays';
export type TokenSubmission = {
  name: string;
  scopes: ManagementTokenScope[];
  expiresInDays: number;
  projectIds: string[] | undefined;
};

export const tokenServerFields: Readonly<Record<string, TokenField>> = {
  name: 'name',
  scopes: 'scopes',
  project_ids: 'projectIds',
  expires_at: 'expiresInDays'
};

export function newTokenDraft(): TokenDraft {
  return {
    name: '',
    scopes: ['read'],
    expiresInDays: 30,
    allProjects: true,
    projectIds: []
  };
}

export function validateToken(
  draft: TokenDraft
): Validation<TokenSubmission, TokenField> {
  const name = draft.name.trim();
  return validated<TokenSubmission, TokenField>(
    {
      name: name ? undefined : 'Enter a token name.',
      scopes: draft.scopes.length ? undefined : 'Select at least one scope.',
      projectIds:
        draft.allProjects || draft.projectIds.length
          ? undefined
          : 'Select at least one project, or choose all projects.'
    },
    () => ({
      name,
      scopes: draft.scopes,
      expiresInDays: draft.expiresInDays,
      projectIds: draft.allProjects ? undefined : draft.projectIds
    })
  );
}
