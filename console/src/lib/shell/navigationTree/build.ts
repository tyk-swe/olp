import { resolve } from '$app/paths';
import type { NavigationSection } from './types.js';

export const BUILD_SECTION: NavigationSection = {
  label: 'Build',
  items: [
    {
      label: 'Providers',
      href: resolve('providers'),
      icon: 'provider',
      route: 'GET /api/v1/providers',
      description:
        'Upstream model vendors and the credentials used to reach them'
    },
    {
      label: 'Models',
      href: resolve('models'),
      icon: 'model',
      description:
        'Models your providers offer, and the catalog you publish to callers',
      tabs: [
        {
          label: 'Provider models',
          href: resolve('models'),
          route: 'GET /api/v1/provider-models'
        },
        {
          label: 'Published catalog',
          href: resolve('models/catalog'),
          route: 'GET /api/v1/catalog'
        }
      ]
    },
    {
      label: 'Routes',
      href: resolve('routes'),
      icon: 'route',
      description: 'Model names callers use, and where each request is sent',
      tabs: [
        {
          label: 'Routes',
          href: resolve('routes'),
          route: 'GET /api/v1/routes'
        },
        {
          label: 'Templates',
          href: resolve('routes/templates'),
          route: 'GET /api/v1/route-templates'
        }
      ]
    },
    {
      label: 'Guardrails',
      href: resolve('guardrails'),
      icon: 'guardrail',
      description: 'Checks that run on prompts and responses',
      tabs: [
        {
          label: 'Checks',
          href: resolve('guardrails/checks'),
          route: 'GET /api/v1/guardrails'
        },
        {
          label: 'Policies',
          href: resolve('guardrails/policies'),
          route: 'GET /api/v1/guardrail-policies'
        },
        {
          label: 'Where they apply',
          href: resolve('guardrails/attachments'),
          route: 'GET /api/v1/guardrail-attachments'
        },
        {
          label: 'Credentials',
          href: resolve('guardrails/credentials'),
          route: 'GET /api/v1/guardrail-credentials'
        },
        {
          label: 'Preview',
          href: resolve('guardrails/preview'),
          route: 'GET /api/v1/guardrails/preview'
        },
        {
          label: 'Try it',
          href: resolve('guardrails/try'),
          route: 'POST /api/v1/guardrails/evaluate'
        }
      ]
    },
    {
      label: 'Code mode',
      href: resolve('code-mode'),
      icon: 'code',
      route: 'GET /api/v1/code/routes',
      description: 'Coding assistants that sign in through this gateway'
    },
    {
      label: 'Plugins',
      href: resolve('plugins'),
      icon: 'plugin',
      route: 'GET /api/v1/plugins',
      controlPlane: true,
      description: 'Extensions that add providers, guardrails and tools'
    }
  ]
};
