<script lang="ts">
  import { onMount } from 'svelte';
  import { resolve } from '$app/paths';
  import type { Snippet } from 'svelte';
  import type { SessionUser } from '$lib/features/access/session/auth';
  import { can } from '$lib/features/access/session/authorization';
  import BrandMark from '$lib/components/BrandMark.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import Navigation from '$lib/components/Navigation.svelte';

  let {
    children,
    user,
    installationName,
    signingOut = false,
    signOutError = '',
    onSignOut
  }: {
    children: Snippet;
    user: SessionUser;
    installationName: string;
    signingOut?: boolean;
    signOutError?: string;
    onSignOut: () => void;
  } = $props();
  const installationLabel = $derived(installationName || 'Local installation');
  let mobileNavigation = $state<HTMLDialogElement>();
  let accountMenu = $state<HTMLDetailsElement>();
  let navigationOpen = $state(false);
  let navigationTrigger = $state<HTMLButtonElement>();
  let desktopBrand = $state<HTMLAnchorElement>();
  let accountMenuOpen = $state(false);

  function openNavigation() {
    mobileNavigation?.showModal();
    navigationOpen = Boolean(mobileNavigation?.open);
  }

  function closeNavigation() {
    mobileNavigation?.close();
    navigationOpen = false;
  }

  onMount(() => {
    const desktop = window.matchMedia('(min-width: 62.001rem)');
    const resize = () => {
      if (desktop.matches && mobileNavigation?.open) {
        closeNavigation();
        desktopBrand?.focus();
      }
    };
    desktop.addEventListener('change', resize);
    return () => desktop.removeEventListener('change', resize);
  });

  function navigationClosed() {
    navigationOpen = false;
    if (
      navigationTrigger &&
      window.getComputedStyle(navigationTrigger).display !== 'none'
    ) {
      navigationTrigger.focus();
    }
  }

  function dismissBackdrop(event: MouseEvent) {
    if (event.target === mobileNavigation) closeNavigation();
  }

  function closeAccountMenu() {
    if (accountMenu) accountMenu.open = false;
  }

  function dismissAccountMenu(event: PointerEvent) {
    if (!accountMenu?.open) return;
    if (event.target instanceof Node && accountMenu.contains(event.target))
      return;
    closeAccountMenu();
  }

  function closeAccountMenuOnEscape(event: KeyboardEvent) {
    if (event.key !== 'Escape' || !accountMenu?.open) return;
    closeAccountMenu();
    accountMenu.querySelector('summary')?.focus();
  }
</script>

<svelte:document
  onpointerdown={dismissAccountMenu}
  onkeydown={closeAccountMenuOnEscape}
/>

<a class="skip-link" href="#main-content">Skip to main content</a>

<div class="shell">
  <header class="topbar">
    <div class="topbar-row">
      <button
        class="menu-button"
        type="button"
        aria-label="Open navigation"
        bind:this={navigationTrigger}
        aria-expanded={navigationOpen}
        aria-haspopup="dialog"
        aria-controls="mobile-navigation"
        onclick={openNavigation}
      >
        <NavIcon name="menu" />
      </button>

      <a
        bind:this={desktopBrand}
        class="brand"
        href={resolve('/')}
        aria-label="OpenLLMProxy overview"
      >
        <BrandMark size={22} />
        <span class="wordmark">OpenLLMProxy</span>
      </a>

      <div class="primary-nav">
        <Navigation role={user.role} variant="bar" />
      </div>

      <div class="topbar-actions">
        <span class="edition" title={installationLabel}
          ><span class="edition-dot" aria-hidden="true"></span><span
            class="edition-name">{installationLabel}</span
          ></span
        >
        <details
          class="account-menu"
          bind:this={accountMenu}
          bind:open={accountMenuOpen}
        >
          <!-- Chromium's accessibility tree does not consistently expose the native summary role. -->
          <!-- svelte-ignore a11y_no_redundant_roles -->
          <summary
            role="button"
            aria-label="Open account menu"
            aria-expanded={accountMenuOpen}
          >
            <span class="avatar" aria-hidden="true"
              >{user?.display_name?.slice(0, 1).toUpperCase() ?? 'A'}</span
            >
            <span class="account-label">{user?.display_name ?? 'Account'}</span>
            <NavIcon name="chevron" size={16} />
          </summary>
          <div class="account-popover">
            <a href={resolve('/settings/profile')} onclick={closeAccountMenu}
              >Personal profile</a
            >
            {#if can(user.role, 'settings.read')}<a
                href={resolve('/settings')}
                onclick={closeAccountMenu}>Installation settings</a
              >{/if}
            <button
              type="button"
              onclick={onSignOut}
              disabled={signingOut}
              aria-busy={signingOut}
            >
              {signingOut ? 'Signing out…' : 'Sign out'}
            </button>
          </div>
        </details>
      </div>
    </div>

    <Navigation role={user.role} variant="subnav" />
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

<dialog
  id="mobile-navigation"
  aria-label="Navigation"
  onclose={navigationClosed}
  class="mobile-dialog"
  bind:this={mobileNavigation}
  onclick={dismissBackdrop}
>
  <div class="mobile-drawer">
    <div class="drawer-heading">
      <a
        class="brand"
        href={resolve('/')}
        aria-label="OpenLLMProxy overview"
        onclick={closeNavigation}
      >
        <BrandMark size={22} />
        <span class="wordmark">OpenLLMProxy</span>
      </a>
      <button
        type="button"
        class="close-button"
        aria-label="Close navigation"
        onclick={closeNavigation}>×</button
      >
    </div>
    <Navigation
      role={user.role}
      label="Mobile primary"
      onNavigate={closeNavigation}
    />
  </div>
</dialog>

<style>
  .shell {
    display: flex;
    min-height: 100dvh;
    flex-direction: column;
  }

  /* The header spans the full viewport so long installation and account names
     truncate inside it instead of pushing the page wider. */
  .topbar {
    position: sticky;
    z-index: 20;
    top: 0;
    padding: 0 1.5rem;
    border-bottom: 1px solid var(--border-hairline);
    background: var(--canvas);
  }

  .topbar-row {
    display: flex;
    min-height: 4rem;
    align-items: center;
    gap: 1rem;
  }

  .brand {
    display: inline-flex;
    min-height: 2.75rem;
    flex: none;
    align-items: center;
    gap: 0.6rem;
    color: var(--foreground);
    text-decoration: none;
  }

  .primary-nav {
    min-width: 0;
    flex: 1 1 auto;
    overflow-x: auto;
    scrollbar-width: none;
  }

  .primary-nav::-webkit-scrollbar {
    display: none;
  }

  .topbar-actions {
    display: flex;
    min-width: 0;
    flex: none;
    align-items: center;
    gap: 0.5rem;
    margin-left: auto;
  }

  .edition {
    display: inline-flex;
    /* An operator may name the installation with up to 100 characters: the
       name is truncated with its full text kept in the tooltip rather than
       pushing the account menu off the edge. */
    overflow: hidden;
    max-width: 14rem;
    min-height: 1.75rem;
    flex: 0 1 auto;
    align-items: center;
    gap: 0.5rem;
    padding: 0 0.6rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    letter-spacing: -0.24px;
    text-transform: uppercase;
    white-space: nowrap;
  }

  .edition-dot {
    width: 6px;
    height: 6px;
    flex: none;
    border-radius: 50%;
    background: var(--signal);
  }

  .edition-name {
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .account-label {
    overflow: hidden;
    max-width: 10rem;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .account-menu {
    position: relative;
  }

  .account-menu summary {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.5rem;
    padding: 0.25rem 0.4rem;
    border-radius: var(--radius-control);
    color: var(--foreground-muted);
    cursor: pointer;
    list-style: none;
    transition:
      background-color var(--motion),
      color var(--motion);
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
    width: 1.75rem;
    height: 1.75rem;
    place-items: center;
    border-radius: var(--radius-control);
    background: var(--foreground);
    color: var(--canvas);
    font-family: var(--font-mono);
    font-size: 0.75rem;
  }

  .account-popover {
    position: absolute;
    z-index: 30;
    top: calc(100% + 0.5rem);
    right: 0;
    display: grid;
    width: 14rem;
    padding: 0.25rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
    background: var(--surface-raised);
  }

  .account-popover a,
  .account-popover button {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 0;
    border-radius: var(--radius-control);
    background: transparent;
    color: inherit;
    text-align: left;
    text-decoration: none;
    transition:
      background-color var(--motion),
      color var(--motion);
  }

  .account-popover a:hover,
  .account-popover button:hover {
    background: color-mix(in srgb, var(--foreground) 8%, var(--surface-raised));
    color: var(--foreground-hover);
  }

  main {
    width: 100%;
    max-width: var(--page-max);
    margin: 0 auto;
    padding: 2rem 1.5rem 6rem;
  }

  .menu-button,
  .close-button {
    display: none;
    width: 2.75rem;
    height: 2.75rem;
    place-items: center;
    border: 1px solid transparent;
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
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
    background: rgb(16 16 16 / 72%);
  }

  .mobile-drawer {
    width: min(90vw, 20rem);
    height: 100dvh;
    overflow-y: auto;
    padding: 1rem 0.75rem;
    border-right: 1px solid var(--border);
    background: var(--canvas);
  }

  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 1.5rem;
  }

  .close-button {
    display: grid;
    font-size: 1.6rem;
    line-height: 1;
  }

  @media (max-width: 80rem) {
    .edition {
      display: none;
    }
  }

  @media (max-width: 62rem) {
    .topbar {
      padding: 0 1rem;
    }

    .topbar-row {
      min-height: 3.5rem;
      gap: 0.5rem;
    }

    .primary-nav {
      display: none;
    }

    .menu-button {
      display: grid;
    }

    .edition {
      display: inline-flex;
      max-width: 10rem;
    }

    .account-label {
      max-width: 7rem;
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

    .brand .wordmark {
      display: none;
    }

    main {
      padding: 1.25rem 1rem 3rem;
    }
  }

  @media (forced-colors: active) {
    .edition-dot {
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
