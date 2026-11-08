import type { User } from './api';

export const ROLE_DETAILS: Record<
  User['role'],
  { label: string; summary: string }
> = {
  owner: {
    label: 'Owner',
    summary: 'Everything, including people, sign-in and plugins'
  },
  operator: {
    label: 'Operator',
    summary: 'Set up providers, routes and installation settings'
  },
  developer: {
    label: 'Developer',
    summary: 'Create API keys, use the playground and see requests'
  },
  viewer: { label: 'Viewer', summary: 'Look around without changing anything' }
};

export const SCOPE_DETAILS: Record<
  User['access_scope'],
  { label: string; summary: string }
> = {
  global: { label: 'All projects', summary: 'Their role applies everywhere' },
  assigned: {
    label: 'Only their projects',
    summary: 'They see projects they are a member of'
  }
};
