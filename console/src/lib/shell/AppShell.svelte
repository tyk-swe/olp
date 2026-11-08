<script lang="ts">
  import { afterNavigate } from '$app/navigation';
  import { page } from '$app/state';
  import type { Snippet } from 'svelte';
  import type { SessionUser } from '#lib/features/access/session/api.js';
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  import Toaster from '#lib/components/Toaster.svelte';
  import BrandMark from '#lib/shell/BrandMark.svelte';
  import CommandPalette from '#lib/shell/CommandPalette.svelte';
  import NavIcon from '#lib/shell/NavIcon.svelte';
  import Sidebar from '#lib/shell/Sidebar.svelte';
  import { breadcrumbs, paletteEntries } from '#lib/shell/navigation.js';

  let {
    children,
    user,
    installationName,
    installationLogo = '',
    signingOut = false,
    signOutError = '',
    onSignOut
  }: {
    children: Snippet;
    user: SessionUser;
    installationName: string;
    installationLogo?: string;
    signingOut?: boolean;
    signOutError?: string;
    onSignOut: () => void;
  } = $props();

  const services = useServiceCapabilities();
  const installationLabel = $derived(installationName || 'Local installation');
  const crumbs = $derived(breadcrumbs(page.url.pathname));
  let paletteOpen = $state(false);
  let drawer = $state<HTMLDialogElement>();

  afterNavigate(() => drawer?.close());

  function shortcut(event: KeyboardEvent) {
    if (event.key.toLowerCase() !== 'k' || !(event.metaKey || event.ctrlKey))
      return;
    event.preventDefault();
    paletteOpen = !paletteOpen;
  }

  function openSearch() {
    drawer?.close();
    paletteOpen = true;
  }
</script>

<svelte:window onkeydown={shortcut} />

<a class="skip-link" href="#main-content">Skip to main content</a>

{#snippet sidebar(onNavigate?: () => void)}
  <Sidebar
    {user}
    installationName={installationLabel}
    {installationLogo}
    {signingOut}
    {onSignOut}
    onSearch={openSearch}
    {onNavigate}
  />
{/snippet}

<div class="shell">
  <div class="rail-cell"><aside class="rail">{@render sidebar()}</aside></div>

  <div class="column">
    <header class="topbar" class:bare={crumbs.length < 2}>
      <button
        class="icon-button menu-button"
        type="button"
        aria-label="Open navigation"
        aria-haspopup="dialog"
        onclick={() => drawer?.showModal()}><NavIcon name="menu" /></button
      >
      <span class="mobile-brand" aria-hidden="true"
        ><BrandMark size={20} /></span
      >
      {#if crumbs.length}
        <nav class="crumbs" aria-label="Breadcrumb">
          <ol>
            {#each crumbs as crumb, index (index)}
              <li>
                {#if crumb.href && index < crumbs.length - 1}<a
                    href={crumb.href}>{crumb.label}</a
                  >{:else}<span
                    aria-current={index === crumbs.length - 1
                      ? 'page'
                      : undefined}>{crumb.label}</span
                  >{/if}
              </li>
            {/each}
          </ol>
        </nav>
      {/if}
      <button
        class="icon-button search-button"
        type="button"
        aria-label="Search the console"
        onclick={openSearch}><NavIcon name="search" /></button
      >
    </header>

    <main id="main-content" tabindex="-1">
      {#if signOutError}
        <div class="problem-banner" role="alert">
          <div>
            <strong>Sign out failed</strong>
            <p>{signOutError} Your session may still be active.</p>
          </div>
          <button
            class="button button-secondary"
            type="button"
            onclick={onSignOut}>Try again</button
          >
        </div>
      {/if}
      {@render children()}
    </main>
  </div>
</div>

<dialog
  class="drawer"
  aria-label="Navigation"
  bind:this={drawer}
  onclick={(event) => {
    if (event.target === drawer) drawer.close();
  }}
>
  <button
    class="icon-button close"
    type="button"
    aria-label="Close navigation"
    onclick={() => drawer?.close()}><NavIcon name="close" /></button
  >
  {@render sidebar(() => drawer?.close())}
</dialog>

{#if paletteOpen}
  <CommandPalette
    entries={paletteEntries(user, services.gatewayAvailable)}
    onClose={() => (paletteOpen = false)}
  />
{/if}

<Toaster />

<style>
  .shell {
    display: grid;
    min-height: 100dvh;
    grid-template-columns: 15.5rem minmax(0, 1fr);
  }
  .rail-cell {
    border-right: 1px solid var(--border-hairline);
    background: color-mix(in srgb, var(--surface-subtle) 60%, var(--canvas));
  }
  .rail {
    position: sticky;
    top: 0;
    height: 100dvh;
  }
  .column {
    display: flex;
    min-width: 0;
    flex-direction: column;
  }
  .topbar {
    position: sticky;
    z-index: 20;
    top: 0;
    display: flex;
    min-height: 3.25rem;
    align-items: center;
    gap: 0.75rem;
    padding: 0 2rem;
    border-bottom: 1px solid var(--border-hairline);
    background: color-mix(in srgb, var(--canvas) 80%, transparent);
    backdrop-filter: blur(16px) saturate(150%);
    -webkit-backdrop-filter: blur(16px) saturate(150%);
  }
  .crumbs {
    min-width: 0;
    flex: 1;
  }
  .crumbs ol {
    display: flex;
    min-width: 0;
    align-items: center;
    gap: 0.45rem;
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--text-body-sm);
  }
  .crumbs li {
    min-width: 0;
    overflow: hidden;
    color: var(--foreground-muted);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .crumbs li + li::before {
    content: '/';
    margin-right: 0.45rem;
    color: var(--border-strong);
  }
  .crumbs a {
    color: var(--foreground-muted);
    text-decoration: none;
  }
  .crumbs a:hover {
    color: var(--foreground-hover);
  }
  .crumbs [aria-current='page'] {
    color: var(--foreground);
  }
  .icon-button {
    display: grid;
    width: 2.5rem;
    height: 2.5rem;
    flex: none;
    place-items: center;
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground-subtle);
  }
  .icon-button:hover {
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }
  .menu-button,
  .search-button,
  .mobile-brand {
    display: none;
  }
  main {
    width: 100%;
    max-width: var(--page-max);
    margin: 0 auto;
    padding: 2rem 2rem 4rem;
    outline: none;
  }
  .drawer {
    width: min(18rem, 86vw);
    max-width: none;
    height: 100dvh;
    max-height: none;
    margin: 0;
    padding: 0;
    border: 0;
    border-right: 1px solid var(--border);
    background: var(--surface);
    color: var(--foreground);
  }
  .drawer[open] {
    animation: slide-in-left var(--dur-enter) var(--ease-out);
  }
  .drawer::backdrop {
    background: var(--backdrop);
  }
  .drawer .close {
    position: absolute;
    top: 0.5rem;
    right: 0.5rem;
    z-index: 1;
  }
  @media (min-width: 64.001rem) {
    .topbar.bare {
      display: none;
    }
  }
  @media (max-width: 64rem) {
    .shell {
      grid-template-columns: minmax(0, 1fr);
    }
    .rail-cell {
      display: none;
    }
    .topbar {
      padding: 0 0.75rem;
    }
    .menu-button,
    .search-button,
    .mobile-brand {
      display: grid;
    }
    main {
      padding: 1.25rem 1rem 3rem;
    }
  }
</style>
