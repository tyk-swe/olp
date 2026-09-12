<script lang="ts">
  import { page } from '$app/state';
  import { resolve } from '$app/paths';
  import {
    can,
    type Capability,
    type FixedRole
  } from '$lib/features/access/session/authorization';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import type { IconName } from '$lib/components/icons';

  type NavigationItem = {
    label: string;
    href: string;
    icon: IconName;
    capability?: Capability;
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
    role,
    variant = 'list',
    label = 'Primary',
    onNavigate
  }: {
    role: FixedRole;
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
          capability: 'configuration.read'
        },
        {
          label: 'Models',
          href: resolve('/models'),
          icon: 'model',
          capability: 'configuration.read'
        },
        {
          label: 'Routes',
          href: resolve('/routes'),
          icon: 'route',
          capability: 'configuration.read'
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
          capability: 'api_keys.read'
        },
        {
          label: 'Access',
          href: resolve('/access'),
          icon: 'access',
          capability: 'users.read'
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
          capability: 'operations.read'
        },
        {
          label: 'Media Jobs',
          href: resolve('/media-jobs'),
          icon: 'request',
          capability: 'operations.read'
        },
        {
          label: 'Usage',
          href: resolve('/usage'),
          icon: 'usage',
          capability: 'operations.read'
        },
        {
          label: 'Health',
          href: resolve('/health'),
          icon: 'health',
          capability: 'operations.read'
        },
        {
          label: 'Audit',
          href: resolve('/audit'),
          icon: 'audit',
          capability: 'operations.read'
        }
      ]
    },
    {
      items: [
        {
          label: 'Playground',
          href: resolve('/playground'),
          icon: 'playground',
          capability: 'playground.use'
        },
        {
          label: 'Settings',
          href: resolve('/settings'),
          icon: 'settings',
          capability: 'settings.read'
        }
      ]
    }
  ];

  function visible(item: NavigationItem) {
    return !item.capability || can(role, item.capability);
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
    <ul>
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
      <ul>
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

  /* Top bar and sub-row: text links with a hairline rule under the current one. */
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
    display: inline-flex;
    min-height: 2.5rem;
    align-items: center;
    padding: 0 0.625rem;
    border-bottom: 1px solid transparent;
    font-size: 0.875rem;
    white-space: nowrap;
  }

  .bar a {
    letter-spacing: 0.02em;
    text-transform: uppercase;
  }

  .subnav {
    display: flex;
    min-height: 3rem;
    align-items: center;
    border-top: 1px solid var(--border-hairline);
  }

  .bar a:hover,
  .subnav a:hover {
    color: var(--foreground);
  }

  .bar a.active,
  .subnav a.active {
    border-bottom-color: var(--foreground);
    color: var(--foreground);
  }

  /* Drawer list: grouped pages with icons under mono group labels. */
  .list {
    display: flex;
    flex-direction: column;
    gap: 1.25rem;
  }
  .nav-group {
    display: grid;
    gap: 0.25rem;
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
    color: var(--foreground);
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
  }
</style>
