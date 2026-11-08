import { resolve } from '$app/paths';
import {
  allows,
  type Grant
} from '#lib/features/access/session/authorization.js';
import type { ManagementRoute } from '#lib/management/requirements.js';
import type { IconName } from '#lib/shell/icons.js';

export type NavigationLeaf = {
  readonly label: string;
  readonly href: string;
  readonly route?: ManagementRoute;
  readonly controlPlane?: true;
};

export type NavigationItem = NavigationLeaf & {
  readonly icon: IconName;
  readonly description?: string;
  readonly tabs?: readonly NavigationLeaf[];
};

export type NavigationSection = {
  readonly label: string;
  readonly items: readonly NavigationItem[];
};

export type VisibleItem = {
  readonly item: NavigationItem;
  readonly href: string;
  readonly tabs: readonly NavigationLeaf[];
};

export type VisibleSection = {
  readonly label: string;
  readonly items: readonly VisibleItem[];
};

export type PaletteEntry = {
  readonly label: string;
  readonly href: string;
  readonly group: string;
  readonly icon: IconName;
};

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

export const NAVIGATION_SECTIONS: readonly NavigationSection[] = [
  {
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
  },
  {
    label: 'Agents',
    items: [
      {
        label: 'Agents and tools',
        href: resolve('agents'),
        icon: 'agent',
        description: 'Toolsets, agents and prompts you publish to callers',
        tabs: [
          {
            label: 'Toolsets',
            href: resolve('agents/toolsets'),
            route: 'GET /api/v1/toolsets'
          },
          {
            label: 'Agents',
            href: resolve('agents/definitions'),
            route: 'GET /api/v1/agents'
          },
          {
            label: 'Prompts',
            href: resolve('agents/prompts'),
            route: 'GET /api/v1/prompts'
          }
        ]
      },
      {
        label: 'MCP servers',
        href: resolve('mcp-servers'),
        icon: 'server',
        route: 'GET /api/v1/mcp-servers',
        description: 'Tool servers your agents and toolsets can call'
      },
      {
        label: 'Activity',
        href: resolve('activity'),
        icon: 'activity',
        description: 'What agents and tools did, and on whose behalf',
        tabs: [
          {
            label: 'Runs',
            href: resolve('activity/runs'),
            route: 'GET /api/v1/agent-runs'
          },
          {
            label: 'Tool calls',
            href: resolve('activity/tool-calls'),
            route: 'GET /api/v1/mcp-tool-calls'
          },
          {
            label: 'Agent-to-agent tasks',
            href: resolve('activity/tasks'),
            route: 'GET /api/v1/a2a-tasks'
          },
          {
            label: 'Delegations',
            href: resolve('activity/delegations'),
            route: 'GET /api/v1/agent-delegations'
          }
        ]
      },
      {
        label: 'Investigations',
        href: resolve('investigations'),
        icon: 'search',
        route: 'GET /api/v1/investigations',
        description: 'Ask an agent to explain what happened in a time window'
      },
      {
        label: 'My connections',
        href: resolve('agents/grants'),
        icon: 'link',
        route: 'GET /api/v1/mcp-user-grants',
        description: 'Tool accounts you have connected for agents to act as you'
      }
    ]
  },
  {
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
  },
  {
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
  },
  {
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
        description:
          'Files, vector stores and other objects created at providers'
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
  }
];

const CREATE_ACTIONS: readonly (PaletteEntry & NavigationLeaf)[] = [
  {
    label: 'Add a provider',
    href: resolve('providers/new'),
    group: 'Create',
    icon: 'provider',
    route: 'POST /api/v1/providers'
  },
  {
    label: 'Create a route',
    href: resolve('routes/new'),
    group: 'Create',
    icon: 'route',
    route: 'POST /api/v1/route-drafts'
  },
  {
    label: 'Create an API key',
    href: resolve('keys/new'),
    group: 'Create',
    icon: 'key',
    route: 'POST /api/v1/api-keys',
    controlPlane: true
  },
  {
    label: 'Invite a member',
    href: resolve('members/invitations'),
    group: 'Create',
    icon: 'access',
    route: 'POST /api/v1/invitations',
    controlPlane: true
  },
  {
    label: 'Start an investigation',
    href: resolve('investigations/new'),
    group: 'Create',
    icon: 'search',
    route: 'POST /api/v1/investigations'
  }
];

const isAtOrBelow = (path: string, ancestor: string) =>
  ancestor === resolve('')
    ? path === ancestor
    : path === ancestor || path.startsWith(`${ancestor}/`);

const leafVisible = (
  leaf: NavigationLeaf,
  grant: Grant,
  gatewayAvailable: boolean
) =>
  (gatewayAvailable || leaf.controlPlane === true) &&
  (!leaf.route || allows(grant, leaf.route));

export function visibleItem(
  item: NavigationItem,
  grant: Grant,
  gatewayAvailable: boolean
): VisibleItem | null {
  const tabs = (item.tabs ?? [item]).filter((leaf) =>
    leafVisible(leaf, grant, gatewayAvailable)
  );
  if (!tabs.length) return null;
  return { item, href: tabs[0].href, tabs: item.tabs ? tabs : [] };
}

export function visibleSections(
  grant: Grant,
  gatewayAvailable: boolean
): readonly VisibleSection[] {
  return NAVIGATION_SECTIONS.flatMap((section) => {
    const items = section.items
      .map((item) => visibleItem(item, grant, gatewayAvailable))
      .filter((item): item is VisibleItem => item !== null);
    return items.length ? [{ label: section.label, items }] : [];
  });
}

const ALL_ITEMS: readonly NavigationItem[] = [
  OVERVIEW,
  PLAYGROUND,
  ...NAVIGATION_SECTIONS.flatMap((section) => section.items),
  SETTINGS,
  PROFILE
];

const sectionOf = (item: NavigationItem) =>
  NAVIGATION_SECTIONS.find((section) => section.items.includes(item))?.label;

export function owningItem(path: string): NavigationItem | undefined {
  const scored = ALL_ITEMS.map((item) => ({
    item,
    depth: Math.max(
      ...[item.href, ...(item.tabs ?? []).map((tab) => tab.href)].map((href) =>
        isAtOrBelow(path, href) ? href.length : -1
      )
    )
  })).filter(({ depth }) => depth >= 0);
  return scored.reduce<(typeof scored)[number] | undefined>(
    (best, entry) => (!best || entry.depth > best.depth ? entry : best),
    undefined
  )?.item;
}

export function activeTab(
  path: string,
  tabs: readonly NavigationLeaf[]
): NavigationLeaf | undefined {
  return tabs
    .filter((tab) => isAtOrBelow(path, tab.href))
    .reduce<NavigationLeaf | undefined>(
      (best, tab) => (!best || tab.href.length > best.href.length ? tab : best),
      undefined
    );
}

export type Crumb = { readonly label: string; readonly href?: string };

export function breadcrumbs(path: string): readonly Crumb[] {
  const item = owningItem(path);
  if (!item || item === OVERVIEW) return [];
  const section = sectionOf(item);
  const tab = activeTab(path, item.tabs ?? []);
  return [
    ...(section ? [{ label: section }] : []),
    { label: item.label, href: item.tabs?.[0].href ?? item.href },
    ...(tab && tab.label !== item.label
      ? [{ label: tab.label, href: tab.href }]
      : [])
  ];
}

export function paletteEntries(
  grant: Grant,
  gatewayAvailable: boolean
): readonly PaletteEntry[] {
  const pinned = [OVERVIEW, PLAYGROUND, SETTINGS, PROFILE]
    .map((item) => visibleItem(item, grant, gatewayAvailable))
    .filter((item): item is VisibleItem => item !== null);
  const pages = [
    ...pinned.map((visible) => ({ section: 'Go to', visible })),
    ...visibleSections(grant, gatewayAvailable).flatMap((section) =>
      section.items.map((visible) => ({ section: section.label, visible }))
    )
  ].flatMap(({ section, visible }) => [
    {
      label: visible.item.label,
      href: visible.href,
      group: section,
      icon: visible.item.icon
    },
    ...visible.tabs
      .filter((tab) => tab.label !== visible.item.label)
      .map((tab) => ({
        label: `${visible.item.label} › ${tab.label}`,
        href: tab.href,
        group: section,
        icon: visible.item.icon
      }))
  ]);
  const actions = CREATE_ACTIONS.filter((action) =>
    leafVisible(action, grant, gatewayAvailable)
  ).map(({ label, href, group, icon }) => ({ label, href, group, icon }));
  return [...actions, ...pages];
}
