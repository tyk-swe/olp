import { resolve } from '$app/paths';
import type { NavigationSection } from './types.js';

export const SPEND_SECTION: NavigationSection = {
  label: 'Spend',
  items: [
    {
      label: 'Usage',
      href: resolve('usage'),
      icon: 'usage',
      description: 'Tokens, requests and cost across your traffic',
      tabs: [
        {
          label: 'Summary',
          href: resolve('usage'),
          route: 'GET /api/v1/usage/summary'
        },
        {
          label: 'Routing savings',
          href: resolve('usage/routing'),
          route: 'GET /api/v1/usage/selector-savings'
        },
        {
          label: 'Reconciliation',
          href: resolve('usage/reconciliation'),
          route: 'GET /api/v1/usage/reconciliation'
        },
        {
          label: 'Cost calculator',
          href: resolve('usage/calculator'),
          route: 'POST /api/v1/pricing/quote'
        }
      ]
    },
    {
      label: 'Budgets',
      href: resolve('budgets'),
      icon: 'wallet',
      description: 'Spending limits and requests to raise them',
      tabs: [
        {
          label: 'Installation limit',
          href: resolve('budgets/limit'),
          route: 'GET /api/v1/budgets/installation',
          controlPlane: true
        },
        {
          label: 'Budget groups',
          href: resolve('budgets/groups'),
          route: 'GET /api/v1/budget-groups',
          controlPlane: true
        },
        {
          label: 'Increase requests',
          href: resolve('budgets/increases'),
          route: 'GET /api/v1/budget-increases'
        },
        {
          label: 'Pending agent spend',
          href: resolve('budgets/exposure'),
          route: 'GET /api/v1/execution-debits'
        }
      ]
    },
    {
      label: 'Pricing',
      href: resolve('pricing'),
      icon: 'tag',
      description: 'How request costs are calculated',
      tabs: [
        {
          label: 'Price lists',
          href: resolve('pricing/sources'),
          route: 'GET /api/v1/pricing/sources'
        },
        {
          label: 'Discounts',
          href: resolve('pricing/discounts'),
          route: 'GET /api/v1/pricing/discounts'
        },
        {
          label: 'Rate cards',
          href: resolve('pricing/rate-cards'),
          route: 'GET /api/v1/rate-cards'
        },
        {
          label: 'Price changes',
          href: resolve('pricing/revisions'),
          route: 'GET /api/v1/pricing/revisions',
          controlPlane: true
        }
      ]
    }
  ]
};
