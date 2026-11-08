import { resolve } from '$app/paths';
import {
  allows,
  type Grant
} from '#lib/features/access/session/authorization.js';
import type { ManagementRoute } from '#lib/management/requirements.js';
import type { IconName } from '#lib/shell/icons.js';

export type NavigationItem = {
  readonly label: string;
  readonly href: string;
  readonly icon: IconName;
  readonly route?: ManagementRoute;
};

export type NavigationGroup = {
  readonly label?: string;
  readonly items: readonly NavigationItem[];
};

export type NavigationEntry = {
  readonly label: string;
  readonly href: string;
  readonly items: readonly NavigationItem[];
};

export const NAVIGATION_GROUPS: readonly NavigationGroup[] = [
  {
    items: [{ label: 'Overview', href: resolve(''), icon: 'overview' }]
  },

  {
    label: 'Gateway',
    items: [
      {
        label: 'Providers',
        href: resolve('providers'),
        icon: 'provider',
        route: 'GET /api/v1/providers'
      },
      {
        label: 'Models',
        href: resolve('provider-models'),
        icon: 'model',
        route: 'GET /api/v1/provider-models'
      },
      {
        label: 'Model catalog',
        href: resolve('catalog'),
        icon: 'model',
        route: 'GET /api/v1/catalog'
      },
      {
        label: 'Routes',
        href: resolve('routes'),
        icon: 'route',
        route: 'GET /api/v1/routes'
      },
      {
        label: 'Guardrails',
        href: resolve('guardrails'),
        icon: 'guardrail',
        route: 'GET /api/v1/guardrails'
      },
      {
        label: 'Plugins',
        href: resolve('plugins'),
        icon: 'provider',
        route: 'GET /api/v1/plugins'
      },
      {
        label: 'Code mode',
        href: resolve('code-mode'),
        icon: 'route',
        route: 'GET /api/v1/code/routes'
      }
    ]
  },
  {
    label: 'Agents',
    items: [
      {
        label: 'Agent publications',
        href: resolve('agents'),
        icon: 'agent',
        route: 'GET /api/v1/toolsets'
      },
      {
        label: 'MCP servers',
        href: resolve('mcp-servers'),
        icon: 'agent',
        route: 'GET /api/v1/mcp-servers'
      },
      {
        label: 'Personal tool grants',
        href: resolve('agents/grants'),
        icon: 'agent',
        route: 'GET /api/v1/mcp-user-grants'
      },
      {
        label: 'Runs',
        href: resolve('runs'),
        icon: 'agent',
        route: 'GET /api/v1/agent-runs'
      },
      {
        label: 'Investigations',
        href: resolve('investigations'),
        icon: 'agent',
        route: 'GET /api/v1/investigations'
      },
      {
        label: 'A2A tasks',
        href: resolve('a2a-tasks'),
        icon: 'agent',
        route: 'GET /api/v1/a2a-tasks'
      },
      {
        label: 'Delegations',
        href: resolve('delegations'),
        icon: 'agent',
        route: 'GET /api/v1/agent-delegations'
      },
      {
        label: 'Tool calls',
        href: resolve('tool-calls'),
        icon: 'agent',
        route: 'GET /api/v1/mcp-tool-calls'
      },
      {
        label: 'Provisional exposure',
        href: resolve('execution-debits'),
        icon: 'agent',
        route: 'GET /api/v1/execution-debits'
      }
    ]
  },
  {
    label: 'Access',
    items: [
      {
        label: 'API keys',
        href: resolve('api-keys'),
        icon: 'key',
        route: 'GET /api/v1/api-keys'
      },
      {
        label: 'SCIM provisioning',
        href: resolve('scim-provisioning'),
        icon: 'access',
        route: 'GET /api/v1/scim/groups'
      },
      {
        label: 'Workload identity',
        href: resolve('workload-issuers'),
        icon: 'access',
        route: 'GET /api/v1/workload-issuers'
      },
      {
        label: 'Organizations',
        href: resolve('organizations'),
        icon: 'access',
        route: 'GET /api/v1/organizations'
      },
      {
        label: 'Budget increases',
        icon: 'access',
        href: resolve('budget-increases'),
        route: 'GET /api/v1/budget-increases'
      },
      {
        label: 'Project policies',
        href: resolve('project-policies'),
        icon: 'access',
        route: 'GET /api/v1/project-memberships'
      },
      {
        label: 'Members',
        href: resolve('access'),
        icon: 'access',
        route: 'GET /api/v1/users'
      }
    ]
  },
  {
    label: 'Operations',
    items: [
      {
        label: 'Requests',
        href: resolve('requests'),
        icon: 'request',
        route: 'GET /api/v1/requests'
      },
      {
        label: 'Usage',
        href: resolve('usage'),
        icon: 'usage',
        route: 'GET /api/v1/usage/summary'
      },
      {
        label: 'Media jobs',
        href: resolve('media-jobs'),
        icon: 'request',
        route: 'GET /api/v1/media-jobs'
      },
      {
        label: 'Provider resources',
        href: resolve('provider-resources'),
        icon: 'request',
        route: 'GET /api/v1/provider-resources'
      },
      {
        label: 'Signed fact exports',
        href: resolve('signed-exports'),
        icon: 'request',
        route: 'GET /api/v1/sinks'
      },
      {
        label: 'Health',
        href: resolve('health'),
        icon: 'health',
        route: 'GET /api/v1/health/ready'
      },
      {
        label: 'Audit',
        href: resolve('audit'),
        icon: 'audit',
        route: 'GET /api/v1/audit'
      }
    ]
  },
  {
    items: [
      {
        label: 'Playground',
        href: resolve('playground'),
        icon: 'playground',
        route: 'POST /api/v1/playground'
      },
      {
        label: 'Settings',
        href: resolve('settings'),
        icon: 'settings',
        route: 'GET /api/v1/settings'
      }
    ]
  }
];

const CONTROL_PLANE_PATHS: readonly string[] = [
  resolve(''),
  resolve('api-keys'),
  resolve('access'),
  resolve('plugins'),
  resolve('audit'),
  resolve('settings')
];

const DESCENDANTS_OWNED_ELSEWHERE: Readonly<Record<string, readonly string[]>> =
  {
    [resolve('agents')]: [resolve('agents/grants')],
    [resolve('settings')]: [resolve('settings/profile')]
  };

const isAtOrBelow = (path: string, ancestor: string) =>
  path === ancestor || path.startsWith(`${ancestor}/`);

export function isActivePath(path: string, href: string): boolean {
  if (href === resolve('')) return path === href;
  if (!isAtOrBelow(path, href)) return false;
  return !DESCENDANTS_OWNED_ELSEWHERE[href]?.some((owned) =>
    isAtOrBelow(path, owned)
  );
}

export function visibleItems(
  group: NavigationGroup,
  grant: Grant,
  gatewayAvailable: boolean
): readonly NavigationItem[] {
  return group.items.filter(
    (item) =>
      (gatewayAvailable || CONTROL_PLANE_PATHS.includes(item.href)) &&
      (!item.route || allows(grant, item.route))
  );
}

// The top bar shows one link per group. Unlabelled groups, and groups a role
// trims to one page, surface that page directly.
export function navigationEntries(
  grant: Grant,
  gatewayAvailable: boolean
): readonly NavigationEntry[] {
  return NAVIGATION_GROUPS.flatMap((group) => {
    const items = visibleItems(group, grant, gatewayAvailable);
    if (!items.length) return [];
    if (!group.label || items.length === 1) {
      return items.map((item) => ({
        label: item.label,
        href: item.href,
        items: [item]
      }));
    }
    return [{ label: group.label, href: items[0].href, items }];
  });
}
