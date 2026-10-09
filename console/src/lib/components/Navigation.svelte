<script lang="ts">
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  const services = useServiceCapabilities();
  import { page } from '$app/state';
  import { resolve } from '$app/paths';
  import type { ManagementRoute } from '$lib/api/requirements';
  import {
    allows,
    type Grant
  } from '$lib/features/access/session/authorization';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import type { IconName } from '$lib/components/icons';
  import { slidingIndicator } from '$lib/components/indicator';

  type NavigationItem = {
    label: string;
    href: string;
    icon: IconName;
    /** The management call the page opens with; hidden unless admitted. */
    route?: ManagementRoute;
  };

  type NavigationGroup = {
    label?: string;
    items: NavigationItem[];
  };

  type NavigationEntry = {
    label: string;
    href: string;
    items: NavigationItem[];
  };

  let {
    grant,
    variant = 'list',
    label = 'Primary',
    onNavigate
  }: {
    grant: Grant;
    variant?: 'bar' | 'subnav' | 'list';
    label?: string;
    onNavigate?: () => void;
  } = $props();

  const groups: NavigationGroup[] = [
    { items: [{ label: 'Overview', href: resolve('/'), icon: 'overview' }] },
    {
      label: 'Gateway',
      items: [
        {
          label: 'Providers',
          href: resolve('/providers'),
          icon: 'provider',
          route: 'GET /api/v1/providers'
        },
        {
          label: 'Plugins',
          href: resolve('/plugins'),
          icon: 'provider',
          route: 'GET /api/v1/plugins'
        },
        {
          label: 'Models',
          href: resolve('/models'),
          icon: 'model',
          route: 'GET /api/v1/provider-models'
        },
        {
          label: 'Routes',
          href: resolve('/routes'),
          icon: 'route',
          route: 'GET /api/v1/routes'
        },
        {
          label: 'Code mode',
          href: resolve('/code-mode'),
          icon: 'route',
          route: 'GET /api/v1/code/routes'
        }
      ]
    },
    {
      label: 'Access',
      items: [
        {
          label: 'API Keys',
          href: resolve('/api-keys'),
          icon: 'key',
          route: 'GET /api/v1/api-keys'
        },
        {
          label: 'SCIM',
          href: resolve('/scim-provisioning'),
          icon: 'access',
          route: 'GET /api/v1/scim/groups'
        },
        {
          label: 'Workload identity',
          href: resolve('/workload-issuers'),
          icon: 'access',
          route: 'GET /api/v1/workload-issuers'
        },
        {
          label: 'Organizations',
          href: resolve('/organizations'),
          icon: 'access',
          route: 'GET /api/v1/organizations'
        },
        {
          label: 'Budget increases',
          icon: 'access',
          href: resolve('/budget-increases'),
          route: 'GET /api/v1/budget-increases'
        },
        {
          label: 'Project policies',
          href: resolve('/project-policies'),
          icon: 'access',
          route: 'GET /api/v1/project-memberships'
        },
        {
          label: 'Access',
          href: resolve('/access'),
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
          href: resolve('/requests'),
          icon: 'request',
          route: 'GET /api/v1/requests'
        },
        {
          label: 'Media Jobs',
          href: resolve('/media-jobs'),
          icon: 'request',
          route: 'GET /api/v1/media-jobs'
        },
        {
          label: 'Provider Resources',
          href: resolve('/provider-resources'),
          icon: 'request',
          route: 'GET /api/v1/provider-resources'
        },
        {
          label: 'Usage',
          href: resolve('/usage'),
          icon: 'usage',
          route: 'GET /api/v1/usage/summary'
        },
        {
          label: 'Health',
          href: resolve('/health'),
          icon: 'health',
          route: 'GET /api/v1/health/ready'
        },
        {
          label: 'Audit',
          href: resolve('/audit'),
          icon: 'audit',
          route: 'GET /api/v1/audit'
        }
      ]
    },
    {
      items: [
        {
          label: 'Playground',
          href: resolve('/playground'),
          icon: 'playground',
          route: 'POST /api/v1/playground'
        },
        {
          label: 'Settings',
          href: resolve('/settings'),
          icon: 'settings',
          route: 'GET /api/v1/settings'
        }
      ]
    }
  ];

  function visible(item: NavigationItem) {
    const controlPaths: string[] = [
      resolve('/'),
      resolve('/api-keys'),
      resolve('/access'),
      resolve('/plugins'),
      resolve('/audit'),
      resolve('/settings')
    ];
    return (
      (services.gatewayAvailable || controlPaths.includes(item.href)) &&
      (!item.route || allows(grant, item.route))
    );
  }

  // Settings owns /settings but not the personal profile beneath it, which has
  // no nav entry of its own and must not light up the installation link.
  const excludedDescendants: Record<string, readonly string[]> = {
    [resolve('/settings')]: [resolve('/settings/profile')]
  };

  function isActive(href: string) {
    const overview = resolve('/');
    if (href === overview) return page.url.pathname === overview;
    const path = page.url.pathname;
    if (path !== href && !path.startsWith(`${href}/`)) return false;
    return !excludedDescendants[href]?.some(
      (excluded) => path === excluded || path.startsWith(`${excluded}/`)
    );
  }

  // The top bar shows one link per group. Unlabelled groups, and groups the
  // role trims to a single page, surface that page directly: a developer sees
  // "API Keys" rather than an "Access" group that only contains it.
  const entries = $derived.by<NavigationEntry[]>(() =>
    groups.flatMap((group) => {
      const items = group.items.filter(visible);
      if (!items.length) return [];
      if (!group.label || items.length === 1) {
        return items.map((item) => ({
          label: item.label,
          href: item.href,
          items: [item]
        }));
      }
      return [{ label: group.label, href: items[0].href, items }];
    })
  );

  const activeEntry = $derived(
    entries.find((entry) => entry.items.some((item) => isActive(item.href)))
  );
</script>

{#if variant === 'bar'}
  <nav class="bar" aria-label={label}>
    <ul
      class="slide-indicator"
      use:slidingIndicator={`${activeEntry?.href}|${entries.length}`}
    >
      {#each entries as entry (entry.href)}
        {@const current = entry.href === activeEntry?.href}
        <li>
          <a
            class:active={current}
            href={entry.href}
            aria-current={current
              ? entry.items.length > 1
                ? 'true'
                : 'page'
              : undefined}
            onclick={onNavigate}>{entry.label}</a
          >
        </li>
      {/each}
    </ul>
  </nav>
{:else if variant === 'subnav'}
  {#if activeEntry && activeEntry.items.length > 1}
    <nav class="subnav" aria-label={activeEntry.label}>
      <ul class="slide-indicator" use:slidingIndicator={page.url.pathname}>
        {#each activeEntry.items as item (item.href)}
          <li>
            <a
              class:active={isActive(item.href)}
              href={item.href}
              aria-current={isActive(item.href) ? 'page' : undefined}
              onclick={onNavigate}>{item.label}</a
            >
          </li>
        {/each}
      </ul>
    </nav>
  {/if}
{:else}
  <nav class="list" aria-label={label}>
    {#each groups as group (group)}
      {@const visibleItems = group.items.filter(visible)}
      {#if visibleItems.length}
        <div class="nav-group">
          {#if group.label}<p class="nav-label">{group.label}</p>{/if}
          <ul>
            {#each visibleItems as item (item.href)}
              <li>
                <a
                  class:active={isActive(item.href)}
                  href={item.href}
                  aria-current={isActive(item.href) ? 'page' : undefined}
                  onclick={onNavigate}
                >
                  <NavIcon name={item.icon} />
                  <span>{item.label}</span>
                </a>
              </li>
            {/each}
          </ul>
        </div>
      {/if}
    {/each}
  </nav>
{/if}

<style>
  ul {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  a {
    color: var(--foreground-muted);
    text-decoration: none;
    transition:
      background-color var(--motion),
      border-color var(--motion),
      color var(--motion);
  }

  /* Top bar and sub-row: text links over one signal indicator that slides to
     the current page. Hovering the row steps the other links back and draws
     a hairline under the hovered one. Every colour step stays above 4.5:1. */
  .bar,
  .subnav {
    min-width: 0;
  }

  .bar ul,
  .subnav ul {
    display: flex;
    gap: 0.25rem;
  }

  .bar a,
  .subnav a {
    position: relative;
    display: inline-flex;
    min-height: 2.5rem;
    align-items: center;
    padding: 0 0.625rem;
    border-bottom: 2px solid transparent;
    color: var(--foreground-subtle);
    font-size: 0.875rem;
    white-space: nowrap;
  }

  .bar a {
    font-size: 0.8125rem;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .bar a::after,
  .subnav a::after {
    content: '';
    position: absolute;
    right: 0.625rem;
    bottom: -2px;
    left: 0.625rem;
    height: 1px;
    background: var(--border-strong);
    transform: scaleX(0);
    transform-origin: left;
    transition: transform 300ms var(--ease-out);
  }

  .subnav {
    display: flex;
    min-height: 3rem;
    align-items: center;
    border-top: 1px solid var(--border-hairline);
  }

  .bar ul:hover a:not(:hover, .active),
  .subnav ul:hover a:not(:hover, .active) {
    color: var(--foreground-muted);
  }

  .bar a:hover,
  .subnav a:hover {
    color: var(--foreground-hover);
  }

  .bar a:not(.active):hover::after,
  .subnav a:not(.active):hover::after {
    transform: scaleX(1);
  }

  .bar a.active,
  .subnav a.active {
    border-bottom-color: var(--signal);
    color: var(--foreground-hover);
  }

  /* Once the shared indicator is placed, the per-link rule steps aside. */
  .bar ul:global([data-indicator]) a.active,
  .subnav ul:global([data-indicator]) a.active {
    border-bottom-color: transparent;
  }

  /* Drawer list: grouped pages with icons under mono group labels. */
  .list {
    display: flex;
    flex-direction: column;
    gap: 1.25rem;
  }
  /* The drawer's groups cascade in each time it opens: the dialog toggles
     their display, which restarts the animation. */
  .nav-group {
    display: grid;
    gap: 0.25rem;
    animation: rise 320ms var(--ease-out) 60ms backwards;
  }
  .nav-group:nth-child(2) {
    animation-delay: 100ms;
  }
  .nav-group:nth-child(3) {
    animation-delay: 140ms;
  }
  .nav-group:nth-child(4) {
    animation-delay: 180ms;
  }
  .nav-group:nth-child(n + 5) {
    animation-delay: 220ms;
  }
  .nav-label {
    margin: 0 0 0.25rem;
    padding: 0 0.5rem;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    font-weight: 400;
    letter-spacing: -0.24px;
    line-height: 1;
    text-transform: uppercase;
  }
  .list ul {
    display: grid;
    gap: 0.125rem;
  }
  .list a {
    display: flex;
    min-height: 2.5rem;
    align-items: center;
    gap: 0.65rem;
    padding: 0.45rem 0.5rem;
    border-radius: var(--radius-control);
    font-size: 0.875rem;
  }
  .list a:hover {
    background: var(--surface-hover);
    color: var(--foreground);
  }
  .list a.active {
    background: var(--surface-raised);
    box-shadow: inset 2px 0 0 0 var(--signal);
    color: var(--foreground-hover);
  }
  .list a.active :global(.icon) {
    color: var(--signal);
  }

  @media (max-width: 62rem) {
    .bar,
    .subnav {
      display: none;
    }
  }

  @media (max-width: 62rem), (pointer: coarse) {
    .list a {
      min-height: 2.75rem;
    }
  }

  @media (forced-colors: active) {
    a.active,
    a:hover {
      outline: 1px solid LinkText;
      outline-offset: -1px;
    }

    .bar a::after,
    .subnav a::after {
      display: none;
    }
  }
</style>
