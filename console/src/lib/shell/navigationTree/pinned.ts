import { resolve } from '$app/paths';
import type { NavigationItem } from './types.js';

export const OVERVIEW: NavigationItem = {
  label: 'Overview',
  href: resolve(''),
  icon: 'overview',
  controlPlane: true
};

export const PLAYGROUND: NavigationItem = {
  label: 'Playground',
  href: resolve('playground'),
  icon: 'playground',
  route: 'POST /api/v1/playground'
};

export const SETTINGS: NavigationItem = {
  label: 'Settings',
  href: resolve('settings'),
  icon: 'settings',
  description: 'Installation-wide defaults that apply to every project',
  tabs: [
    {
      label: 'General',
      href: resolve('settings/general'),
      route: 'GET /api/v1/settings',
      controlPlane: true
    },
    {
      label: 'Sign-in & security',
      href: resolve('settings/security'),
      route: 'GET /api/v1/settings',
      controlPlane: true
    },
    {
      label: 'Data retention',
      href: resolve('settings/retention'),
      route: 'GET /api/v1/settings',
      controlPlane: true
    },
    {
      label: 'Default routing',
      href: resolve('settings/routing'),
      route: 'GET /api/v1/settings'
    },
    {
      label: 'Import & export',
      href: resolve('settings/configuration'),
      route: 'GET /api/v1/configuration/export',
      controlPlane: true
    }
  ]
};

export const PROFILE: NavigationItem = {
  label: 'Your profile',
  href: resolve('settings/profile'),
  icon: 'access',
  controlPlane: true
};
