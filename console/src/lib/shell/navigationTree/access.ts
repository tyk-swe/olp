import { resolve } from '$app/paths';
import type { NavigationSection } from './types.js';

export const ACCESS_SECTION: NavigationSection = {
  label: 'Access',
  items: [
    {
      label: 'API keys',
      href: resolve('keys'),
      icon: 'key',
      route: 'GET /api/v1/api-keys',
      controlPlane: true,
      description: 'Keys that applications use to call the gateway'
    },
    {
      label: 'Projects',
      href: resolve('projects'),
      icon: 'folder',
      route: 'GET /api/v1/project-memberships',
      controlPlane: true,
      description: 'Groups of keys, routes and budgets owned by one team'
    },
    {
      label: 'Organizations',
      href: resolve('organizations'),
      icon: 'building',
      route: 'GET /api/v1/organizations',
      description: 'The top level that projects belong to'
    },
    {
      label: 'Members',
      href: resolve('members'),
      icon: 'access',
      description: 'People who can sign in to this console',
      tabs: [
        {
          label: 'People',
          href: resolve('members/people'),
          route: 'GET /api/v1/users',
          controlPlane: true
        },
        {
          label: 'Invitations',
          href: resolve('members/invitations'),
          route: 'GET /api/v1/invitations',
          controlPlane: true
        },
        {
          label: 'Sessions',
          href: resolve('members/sessions'),
          route: 'GET /api/v1/sessions',
          controlPlane: true
        }
      ]
    },
    {
      label: 'Identity',
      href: resolve('identity'),
      icon: 'id',
      description: 'Single sign-on, directory sync and machine identities',
      tabs: [
        {
          label: 'Single sign-on',
          href: resolve('identity/sso'),
          route: 'GET /api/v1/oidc/configuration',
          controlPlane: true
        },
        {
          label: 'Directory sync',
          href: resolve('identity/scim'),
          route: 'GET /api/v1/scim/groups'
        },
        {
          label: 'Workload identity',
          href: resolve('identity/workload'),
          route: 'GET /api/v1/workload-issuers'
        },
        {
          label: 'Management tokens',
          href: resolve('identity/tokens'),
          route: 'GET /api/v1/management-tokens',
          controlPlane: true
        }
      ]
    }
  ]
};
