import { resolve } from '$app/paths';
import type { NavigationSection } from './types.js';

export const OBSERVE_SECTION: NavigationSection = {
  label: 'Observe',
  items: [
    {
      label: 'Requests',
      href: resolve('requests'),
      icon: 'request',
      route: 'GET /api/v1/requests',
      description: 'Every request the gateway handled'
    },
    {
      label: 'Media jobs',
      href: resolve('media-jobs'),
      icon: 'media',
      route: 'GET /api/v1/media-jobs',
      description: 'Long-running image, video and batch jobs'
    },
    {
      label: 'Provider resources',
      href: resolve('provider-resources'),
      icon: 'box',
      route: 'GET /api/v1/provider-resources',
      description: 'Files, vector stores and other objects created at providers'
    },
    {
      label: 'Notifications',
      href: resolve('notifications'),
      icon: 'bell',
      description: 'Alerts sent when budgets or keys need attention',
      tabs: [
        {
          label: 'Destinations',
          href: resolve('notifications/destinations'),
          route: 'GET /api/v1/notifications/destinations',
          controlPlane: true
        },
        {
          label: 'Rules',
          href: resolve('notifications/rules'),
          route: 'GET /api/v1/notifications/rules',
          controlPlane: true
        },
        {
          label: 'Delivery log',
          href: resolve('notifications/deliveries'),
          route: 'GET /api/v1/notifications/deliveries',
          controlPlane: true
        }
      ]
    },
    {
      label: 'Exports',
      href: resolve('exports'),
      icon: 'export',
      description:
        'Send logs, traces and signed usage records to other systems',
      tabs: [
        {
          label: 'Telemetry',
          href: resolve('exports/telemetry'),
          route: 'GET /api/v1/observability/sinks',
          controlPlane: true
        },
        {
          label: 'Prompt capture',
          href: resolve('exports/capture'),
          route: 'GET /api/v1/observability/capture-policies',
          controlPlane: true
        },
        {
          label: 'Signed usage records',
          href: resolve('exports/signed'),
          route: 'GET /api/v1/sinks'
        }
      ]
    },
    {
      label: 'Health',
      href: resolve('health'),
      icon: 'health',
      route: 'GET /api/v1/health/ready',
      description: 'Whether the gateway and its providers are working'
    },
    {
      label: 'Audit log',
      href: resolve('audit'),
      icon: 'audit',
      route: 'GET /api/v1/audit',
      controlPlane: true,
      description: 'Who changed what, and when'
    }
  ]
};
