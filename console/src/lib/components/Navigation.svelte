<script lang="ts">
  import { page } from '$app/state';
  import { resolve } from '$app/paths';
  import NavIcon from './NavIcon.svelte';
  import type { IconName } from './icons';

  type NavigationItem = {
    label: string;
    href: string;
    icon: IconName;
  };

  type NavigationGroup = {
    label?: string;
    items: NavigationItem[];
  };

  let {
    label = 'Primary',
    onNavigate
  }: { label?: string; onNavigate?: () => void } = $props();

  const groups: NavigationGroup[] = [
    { items: [{ label: 'Overview', href: resolve('/'), icon: 'overview' }] },
    {
      label: 'Gateway',
      items: [
        { label: 'Providers', href: resolve('/providers'), icon: 'provider' },
        { label: 'Models', href: resolve('/models'), icon: 'model' },
        { label: 'Routes', href: resolve('/routes'), icon: 'route' }
      ]
    },
    {
      label: 'Access',
      items: [
        { label: 'API Keys', href: resolve('/api-keys'), icon: 'key' },
        { label: 'Access', href: resolve('/access'), icon: 'access' }
      ]
    },
    {
      label: 'Operations',
      items: [
        { label: 'Requests', href: resolve('/requests'), icon: 'request' },
        { label: 'Media Jobs', href: resolve('/media-jobs'), icon: 'request' },
        { label: 'Usage', href: resolve('/usage'), icon: 'usage' },
        { label: 'Health', href: resolve('/health'), icon: 'health' },
        { label: 'Audit', href: resolve('/audit'), icon: 'audit' }
      ]
    },
    {
      items: [
        { label: 'Playground', href: resolve('/playground'), icon: 'playground' },
        { label: 'Settings', href: resolve('/settings'), icon: 'settings' }
      ]
    }
  ];

  function isActive(href: string) {
    const overview = resolve('/');
    return href === overview ? page.url.pathname === overview : page.url.pathname.startsWith(href);
  }
</script>

<nav aria-label={label}>
  {#each groups as group (group)}
    <div class="nav-group">
      {#if group.label}<p class="nav-label">{group.label}</p>{/if}
      <ul>
        {#each group.items as item (item.href)}
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
  {/each}
</nav>

<style>
  nav {
    display: flex;
    flex-direction: column;
    gap: 1.1rem;
  }

  .nav-group {
    display: grid;
    gap: 0.2rem;
  }

  .nav-label {
    margin: 0 0 0.15rem;
    padding: 0 0.55rem;
    color: var(--sidebar-label);
    font-size: 0.6563rem;
    font-weight: 760;
    letter-spacing: 0.1em;
    text-transform: uppercase;
  }

  ul {
    display: grid;
    gap: 0.125rem;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  a {
    display: flex;
    min-height: 2.375rem;
    align-items: center;
    gap: 0.65rem;
    padding: 0.45rem 0.55rem;
    border-radius: 0.375rem;
    color: var(--sidebar-foreground);
    font-size: 0.8125rem;
    font-weight: 620;
    text-decoration: none;
  }

  a:hover {
    background: var(--sidebar-hover);
    color: var(--sidebar-foreground-strong);
  }

  a.active {
    background: var(--sidebar-active);
    color: #ffffff;
    font-weight: 680;
  }

  @media (forced-colors: active) {
    a.active,
    a:hover {
      outline: 1px solid LinkText;
      outline-offset: -1px;
    }
  }
</style>
