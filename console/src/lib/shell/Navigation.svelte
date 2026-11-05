<script lang="ts">
  import { page } from '$app/state';
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  import type { Grant } from '$lib/features/access/session/authorization';
  import NavIcon from '$lib/shell/NavIcon.svelte';
  import { slidingIndicator } from '$lib/shell/indicator';
  import {
    isActivePath,
    NAVIGATION_GROUPS,
    navigationEntries,
    visibleItems
  } from '$lib/shell/navigation';

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

  const services = useServiceCapabilities();
  const isActive = (href: string) => isActivePath(page.url.pathname, href);
  const entries = $derived(navigationEntries(grant, services.gatewayAvailable));
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
              onfocus={(event) =>
                event.currentTarget.scrollIntoView({
                  block: 'nearest',
                  inline: 'nearest'
                })}
              onclick={onNavigate}>{item.label}</a
            >
          </li>
        {/each}
      </ul>
    </nav>
  {/if}
{:else}
  <nav class="list" aria-label={label}>
    {#each NAVIGATION_GROUPS as group (group)}
      {@const items = visibleItems(group, grant, services.gatewayAvailable)}
      {#if items.length}
        <div class="nav-group">
          {#if group.label}<p class="nav-label">{group.label}</p>{/if}
          <ul>
            {#each items as item (item.href)}
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

  .subnav a {
    padding-inline: 0.4375rem;
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
    overflow-x: auto;
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

  @media (max-width: 74rem) {
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
