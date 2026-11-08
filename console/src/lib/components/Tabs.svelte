<script lang="ts">
  import { page } from '$app/state';
  import { slidingIndicator } from '#lib/shell/indicator.js';
  import { activeTab, type NavigationLeaf } from '#lib/shell/navigation.js';
  let { tabs, label }: { tabs: readonly NavigationLeaf[]; label: string } =
    $props();
  const current = $derived(activeTab(page.url.pathname, tabs)?.href);
</script>

{#if tabs.length > 1}
  <nav class="tabs" aria-label={label}>
    <ul class="slide-indicator" use:slidingIndicator={current}>
      {#each tabs as tab (tab.href)}
        <li>
          <a
            class:active={tab.href === current}
            href={tab.href}
            aria-current={tab.href === current ? 'page' : undefined}
            >{tab.label}</a
          >
        </li>
      {/each}
    </ul>
  </nav>
{/if}

<style>
  .tabs {
    margin: -0.5rem 0 1.5rem;
    overflow-x: auto;
    border-bottom: 1px solid var(--border-hairline);
    scrollbar-width: none;
  }

  ul {
    display: flex;
    gap: 0.25rem;
  }

  a {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    padding: 0 0.75rem;
    border-bottom: 2px solid transparent;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
    text-decoration: none;
    white-space: nowrap;
    transition: color var(--motion);
  }

  a:hover {
    color: var(--foreground-hover);
  }

  a.active {
    border-bottom-color: var(--signal);
    color: var(--foreground);
  }

  ul:global([data-indicator]) a.active {
    border-bottom-color: transparent;
  }
</style>
