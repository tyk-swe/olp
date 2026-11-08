<script lang="ts">
  import { page } from '$app/state';
  import type { SessionUser } from '#lib/features/access/session/api.js';
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  import InstallationSwitcher from '#lib/features/fleet/InstallationSwitcher.svelte';
  import Menu from '#lib/components/Menu.svelte';
  import BrandMark from '#lib/shell/BrandMark.svelte';
  import NavIcon from '#lib/shell/NavIcon.svelte';
  import {
    OVERVIEW,
    PLAYGROUND,
    PROFILE,
    SETTINGS,
    owningItem,
    visibleItem,
    visibleSections,
    type VisibleItem
  } from '#lib/shell/navigation.js';

  let {
    user,
    installationName,
    installationLogo,
    signingOut,
    onSignOut,
    onSearch,
    onNavigate = () => {}
  }: {
    user: SessionUser;
    installationName: string;
    installationLogo: string;
    signingOut: boolean;
    onSignOut: () => void;
    onSearch: () => void;
    onNavigate?: () => void;
  } = $props();

  const services = useServiceCapabilities();
  const sections = $derived(visibleSections(user, services.gatewayAvailable));
  const pinned = $derived(
    [OVERVIEW, PLAYGROUND]
      .map((item) => visibleItem(item, user, services.gatewayAvailable))
      .filter((item): item is VisibleItem => item !== null)
  );
  const settings = $derived(
    visibleItem(SETTINGS, user, services.gatewayAvailable)
  );
  const current = $derived(owningItem(page.url.pathname));
  const isMac = /Mac|iPhone|iPad/.test(globalThis.navigator?.platform ?? '');
</script>

{#snippet link(entry: VisibleItem)}
  <li>
    <a
      href={entry.href}
      class:active={current === entry.item}
      aria-current={current === entry.item ? 'page' : undefined}
      onclick={onNavigate}
    >
      <NavIcon name={entry.item.icon} size={18} />
      <span>{entry.item.label}</span>
    </a>
  </li>
{/snippet}

<div class="sidebar">
  <div class="identity">
    {#if installationLogo}<img
        class="logo"
        src={installationLogo}
        alt=""
      />{:else}<BrandMark size={22} />{/if}
    <InstallationSwitcher name={installationName} />
  </div>

  <button class="search" type="button" onclick={onSearch}>
    <NavIcon name="search" size={16} />
    <span>Search</span>
    <kbd>{isMac ? '⌘' : 'Ctrl'} K</kbd>
  </button>

  <nav aria-label="Primary">
    <ul>
      {#each pinned as entry (entry.item.href)}{@render link(entry)}{/each}
    </ul>
    {#each sections as section (section.label)}
      <h2>{section.label}</h2>
      <ul>
        {#each section.items as entry (entry.item.href)}{@render link(
            entry
          )}{/each}
      </ul>
    {/each}
  </nav>

  <div class="footer">
    {#if settings}<ul>{@render link(settings)}</ul>{/if}
    <div class="account">
      <a
        class="person"
        href={PROFILE.href}
        class:active={current === PROFILE}
        aria-current={current === PROFILE ? 'page' : undefined}
        onclick={onNavigate}
      >
        <span class="avatar" aria-hidden="true"
          >{user.display_name.slice(0, 1).toUpperCase()}</span
        >
        <span class="who">
          <strong>{user.display_name}</strong>
          <small>{user.role}</small>
        </span>
      </a>
      <Menu label="Account">
        <a href={PROFILE.href} onclick={onNavigate}>Your profile</a>
        <hr />
        <button
          type="button"
          onclick={onSignOut}
          disabled={signingOut}
          aria-busy={signingOut}
          >{signingOut ? 'Signing out…' : 'Sign out'}</button
        >
      </Menu>
    </div>
  </div>
</div>

<style>
  .sidebar {
    display: flex;
    height: 100%;
    flex-direction: column;
    gap: 0.75rem;
    padding: 0.9rem 0.75rem 0.75rem;
  }
  .identity {
    display: flex;
    min-width: 0;
    align-items: center;
    gap: 0.55rem;
    padding: 0 0.35rem;
  }
  .logo {
    width: 22px;
    height: 22px;
    flex: none;
    border-radius: 5px;
    object-fit: contain;
  }
  .search {
    display: flex;
    min-height: 2.25rem;
    align-items: center;
    gap: 0.55rem;
    padding: 0 0.6rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: var(--surface-subtle);
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
    text-align: left;
    transition:
      border-color var(--motion),
      color var(--motion);
  }
  .search:hover {
    border-color: var(--border-strong);
    color: var(--foreground-hover);
  }
  .search span {
    flex: 1;
  }
  kbd {
    padding: 0.05rem 0.35rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    color: var(--foreground-muted);
    font-family: var(--font-mono);
    font-size: 0.68rem;
  }
  nav {
    min-height: 0;
    flex: 1;
    margin: 0 -0.75rem;
    padding: 0 0.75rem 1rem;
    overflow-y: auto;
    scrollbar-width: thin;
  }
  h2 {
    margin: 1.1rem 0 0.3rem;
    padding: 0 0.6rem;
    color: var(--foreground-muted);
    font-family: var(--font-mono);
    font-size: 0.68rem;
    font-weight: 500;
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }
  ul {
    display: grid;
    gap: 1px;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li a,
  .person {
    display: flex;
    min-height: 2.1rem;
    align-items: center;
    gap: 0.65rem;
    padding: 0 0.6rem;
    border-radius: var(--radius-control);
    color: var(--foreground-subtle);
    font-size: var(--text-body-sm);
    text-decoration: none;
    transition:
      background-color var(--motion),
      color var(--motion);
  }
  li a:hover,
  .person:hover {
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }
  li a.active,
  .person.active {
    background: var(--accent-soft);
    color: var(--foreground);
  }
  li a.active :global(.icon) {
    color: var(--accent);
  }
  li a :global(.icon) {
    flex: none;
    color: var(--foreground-muted);
  }
  .footer {
    display: grid;
    gap: 0.5rem;
    padding-top: 0.6rem;
    border-top: 1px solid var(--border-hairline);
  }
  .account {
    display: flex;
    align-items: center;
    gap: 0.35rem;
  }
  .person {
    min-width: 0;
    flex: 1;
    min-height: 2.75rem;
  }
  .avatar {
    display: grid;
    width: 1.75rem;
    height: 1.75rem;
    flex: none;
    place-items: center;
    border-radius: 50%;
    background: var(--accent-soft);
    color: var(--accent);
    font-size: 0.8rem;
    font-weight: 600;
  }
  .who {
    display: grid;
    min-width: 0;
  }
  .who strong,
  .who small {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .who strong {
    color: var(--foreground);
    font-weight: 500;
  }
  .who small {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
    text-transform: capitalize;
  }
</style>
