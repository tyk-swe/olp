<script lang="ts">
  import { useRole } from '$lib/auth/useRole.svelte';
  import InvitationsPanel from './invitations/InvitationsPanel.svelte';
  import OidcConfigurationPanel from './oidc/OidcConfigurationPanel.svelte';
  import SessionsPanel from './sessions/SessionsPanel.svelte';
  import MembersPanel from './users/MembersPanel.svelte';

  type Tab = 'members' | 'invitations' | 'sessions' | 'oidc';
  const tabs: ReadonlyArray<{ id: Tab; label: string }> = [
    { id: 'members', label: 'Members' },
    { id: 'invitations', label: 'Invitations' },
    { id: 'sessions', label: 'Sessions' },
    { id: 'oidc', label: 'OIDC' }
  ];
  const access = useRole();
  const canManage = $derived(access.can('users.manage'));
  let tab = $state<Tab>('members');
</script>

<svelte:head><title>Access · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Identity</p>
    <h1 class="page-title">Access</h1>
    <p class="page-description">
      Manage installation members, fixed roles, invitations, sessions, and the
      linked OIDC provider.
    </p>
  </div>
  {#if canManage}
    <button
      class="button button-primary"
      type="button"
      onclick={() => (tab = 'invitations')}>Invite member</button
    >
  {/if}
</div>

<nav class="tabs" aria-label="Access settings">
  {#each tabs as item (item.id)}
    <button
      class:active={tab === item.id}
      type="button"
      aria-current={tab === item.id ? 'page' : undefined}
      onclick={() => (tab = item.id)}>{item.label}</button
    >
  {/each}
</nav>

{#if tab === 'members'}
  <MembersPanel />
{:else if tab === 'invitations'}
  <InvitationsPanel />
{:else if tab === 'sessions'}
  <SessionsPanel />
{:else}
  <OidcConfigurationPanel />
{/if}

<style>
  .tabs {
    display: flex;
    gap: 0.25rem;
    margin: 1.5rem 0;
    overflow-x: auto;
    border-bottom: 1px solid var(--border);
  }
  .tabs button {
    min-height: 2.75rem;
    padding: 0.65rem 0.85rem;
    border: 0;
    border-bottom: 2px solid transparent;
    background: transparent;
    color: var(--foreground-muted);
    white-space: nowrap;
  }
  .tabs button.active {
    border-color: var(--accent);
    color: var(--accent-strong);
    font-weight: 750;
  }
</style>
