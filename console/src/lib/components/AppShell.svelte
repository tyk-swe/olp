<script lang="ts">
  import { resolve } from '$app/paths';
  import type { Snippet } from 'svelte';
  import type { SessionUser } from '$lib/api/auth';
  import BrandMark from './BrandMark.svelte';
  import NavIcon from './NavIcon.svelte';
  import Navigation from './Navigation.svelte';
  import ThemeToggle from './ThemeToggle.svelte';

  let {
    children,
    user,
    signingOut = false,
    signOutError = '',
    onSignOut
  }: {
    children: Snippet;
    user: SessionUser;
    signingOut?: boolean;
    signOutError?: string;
    onSignOut: () => void;
  } = $props();
  let mobileNavigation = $state<HTMLDialogElement>();

  function openNavigation() {
    mobileNavigation?.showModal();
  }

  function closeNavigation() {
    mobileNavigation?.close();
  }

  function dismissBackdrop(event: MouseEvent) {
    if (event.target === mobileNavigation) closeNavigation();
  }

</script>

<a class="skip-link" href="#main-content">Skip to main content</a>

<div class="shell">
  <aside class="desktop-sidebar">
    <a class="brand" href={resolve('/')} aria-label="OpenLLMProxy overview">
      <BrandMark />
      <span>OpenLLMProxy</span>
    </a>
    <Navigation />
    <div class="sidebar-footer">
      <span class="environment-dot" aria-hidden="true"></span>
      <span>Self-hosted</span>
    </div>
  </aside>

  <div class="workspace">
    <header class="topbar">
      <button class="menu-button" type="button" aria-label="Open navigation" aria-controls="mobile-navigation" onclick={openNavigation}>
        <NavIcon name="menu" />
      </button>

      <div class="mobile-brand" aria-hidden="true">
        <BrandMark size={28} />
        <span>OpenLLMProxy</span>
      </div>

      <div class="topbar-actions">
        <span class="edition"><span aria-hidden="true"></span>Local installation</span>
        <ThemeToggle />
        <details class="account-menu">
          <!-- Chromium's accessibility tree does not consistently expose the native summary role. -->
          <!-- svelte-ignore a11y_no_redundant_roles -->
          <summary role="button" aria-label="Open account menu">
            <span class="avatar" aria-hidden="true">{user?.display_name?.slice(0, 1).toUpperCase() ?? 'A'}</span>
            <span class="account-label">{user?.display_name ?? 'Account'}</span>
            <NavIcon name="chevron" size={16} />
          </summary>
          <div class="account-popover">
            <a href={resolve('/settings/profile')}>Personal profile</a>
            <a href={resolve('/settings')}>Installation settings</a>
            <button type="button" onclick={onSignOut} disabled={signingOut} aria-busy={signingOut}>
              {signingOut ? 'Signing out…' : 'Sign out'}
            </button>
          </div>
        </details>
      </div>
    </header>

    <main id="main-content" tabindex="-1">
      {#if signOutError}
        <div class="problem-banner" role="alert">
          <div>
            <strong>Sign out failed</strong>
            <p>{signOutError} Your session may still be active.</p>
          </div>
          <button class="button button-secondary" type="button" onclick={onSignOut}>Try again</button>
        </div>
      {/if}
      {@render children()}
    </main>
  </div>
</div>

<dialog id="mobile-navigation" class="mobile-dialog" bind:this={mobileNavigation} onclick={dismissBackdrop}>
  <div class="mobile-drawer">
    <div class="drawer-heading">
      <a class="brand" href={resolve('/')} aria-label="OpenLLMProxy overview" onclick={closeNavigation}>
        <BrandMark />
        <span>OpenLLMProxy</span>
      </a>
      <button type="button" class="close-button" aria-label="Close navigation" onclick={closeNavigation}>×</button>
    </div>
    <Navigation label="Mobile primary" onNavigate={closeNavigation} />
  </div>
</dialog>

<style>
  .shell {
    display: grid;
    min-height: 100dvh;
    grid-template-columns: 15.5rem minmax(0, 1fr);
  }

  .desktop-sidebar {
    position: sticky;
    top: 0;
    display: flex;
    width: 15.5rem;
    height: 100dvh;
    flex-direction: column;
    gap: 1.25rem;
    overflow-y: auto;
    padding: 1rem 0.75rem;
    border-right: 1px solid var(--sidebar-border);
    background: var(--sidebar-bg);
  }

  .brand {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.7rem;
    padding: 0 0.4rem;
    color: var(--sidebar-foreground-strong);
    font-size: 0.975rem;
    font-weight: 730;
    letter-spacing: -0.015em;
    text-decoration: none;
  }

  .desktop-sidebar .brand,
  .mobile-drawer .brand {
    --accent: #4d8bfd;
  }

  .sidebar-footer {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.5rem;
    margin-top: auto;
    padding: 0.65rem 0.4rem 0;
    border-top: 1px solid var(--sidebar-border);
    color: var(--sidebar-foreground);
    font-size: 0.75rem;
  }

  .environment-dot,
  .edition span {
    width: 0.5rem;
    height: 0.5rem;
    border-radius: 999px;
    background: var(--success);
  }

  .workspace {
    min-width: 0;
  }

  .topbar {
    position: sticky;
    z-index: 20;
    top: 0;
    display: flex;
    height: 3.5rem;
    align-items: center;
    justify-content: flex-end;
    padding: 0 1.75rem;
    border-bottom: 1px solid var(--border);
    background: color-mix(in srgb, var(--surface) 94%, transparent);
    backdrop-filter: blur(14px);
  }

  .topbar-actions {
    display: flex;
    align-items: center;
    gap: 0.35rem;
  }

  .edition {
    display: inline-flex;
    min-height: 1.75rem;
    align-items: center;
    gap: 0.45rem;
    margin-right: 0.5rem;
    padding: 0.15rem 0.55rem;
    border: 1px solid var(--border);
    border-radius: 0.25rem;
    background: var(--surface-subtle);
    color: var(--foreground-muted);
    font-size: 0.6875rem;
    font-weight: 700;
    letter-spacing: 0.045em;
    text-transform: uppercase;
  }

  .account-menu {
    position: relative;
  }

  .account-menu summary {
    display: flex;
    min-height: 2.5rem;
    align-items: center;
    gap: 0.5rem;
    padding: 0.25rem 0.4rem;
    border-radius: 0.375rem;
    color: var(--foreground-muted);
    cursor: pointer;
    font-weight: 650;
    list-style: none;
  }

  .account-menu summary::-webkit-details-marker {
    display: none;
  }

  .account-menu summary:hover {
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }

  .avatar {
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    border-radius: 0.375rem;
    background: var(--sidebar-bg);
    color: #fff;
    font-size: 0.75rem;
    font-weight: 800;
  }

  .account-popover {
    position: absolute;
    z-index: 30;
    top: calc(100% + 0.45rem);
    right: 0;
    display: grid;
    width: 14rem;
    padding: 0.3rem;
    border: 1px solid var(--border);
    border-radius: 0.5rem;
    background: var(--surface);
    box-shadow: var(--shadow-md);
  }

  .account-popover a,
  .account-popover button {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.55rem 0.65rem;
    border: 0;
    border-radius: 0.3rem;
    background: transparent;
    color: inherit;
    text-align: left;
    text-decoration: none;
  }

  .account-popover a:hover,
  .account-popover button:hover {
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }

  main {
    width: 100%;
    max-width: 98rem;
    margin: 0 auto;
    padding: clamp(1.5rem, 3.5vw, 3rem);
  }

  .menu-button,
  .close-button {
    display: none;
    width: 2.75rem;
    height: 2.75rem;
    place-items: center;
    border: 1px solid transparent;
    border-radius: 0.375rem;
    background: transparent;
    color: var(--foreground);
  }

  .mobile-brand {
    display: none;
    align-items: center;
    gap: 0.55rem;
    font-weight: 730;
    letter-spacing: -0.015em;
  }

  .mobile-dialog {
    width: 100%;
    max-width: none;
    height: 100%;
    max-height: none;
    margin: 0;
    padding: 0;
    border: 0;
    background: transparent;
  }

  .mobile-dialog::backdrop {
    background: rgb(4 12 26 / 62%);
    backdrop-filter: blur(2px);
  }

  .mobile-drawer {
    width: min(90vw, 20rem);
    height: 100dvh;
    overflow-y: auto;
    padding: 1rem 0.75rem;
    border-right: 1px solid var(--sidebar-border);
    background: var(--sidebar-bg);
    box-shadow: var(--shadow-md);
  }

  .mobile-drawer .close-button {
    color: var(--sidebar-foreground-strong);
  }

  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 1.25rem;
  }

  .close-button {
    display: grid;
    font-size: 1.6rem;
    line-height: 1;
  }

  @media (max-width: 62rem) {
    .shell {
      display: block;
    }

    .desktop-sidebar {
      display: none;
    }

    .topbar {
      justify-content: space-between;
      padding: 0 1rem;
    }

    .menu-button,
    .mobile-brand {
      display: grid;
    }

    .mobile-brand {
      display: flex;
      margin-right: auto;
      margin-left: 0.5rem;
    }
  }

  @media (max-width: 40rem) {
    .edition,
    .account-label,
    .account-menu :global(svg) {
      display: none;
    }

    .topbar {
      padding: 0 0.75rem;
    }

    main {
      padding: 1.25rem 1rem 2rem;
    }

    .mobile-brand span {
      display: none;
    }
  }

  @media (forced-colors: active) {
    .environment-dot,
    .edition span {
      border: 1px solid CanvasText;
      background: CanvasText;
    }

    .account-popover a:hover,
    .account-popover button:hover {
      outline: 1px solid CanvasText;
      outline-offset: -1px;
    }
  }
</style>
