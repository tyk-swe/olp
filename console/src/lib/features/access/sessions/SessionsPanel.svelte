<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { logout } from '$lib/api/auth';
  import {
    listSessionPage,
    listUserPage,
    revokeSession
  } from '$lib/api/management/access';
  import { authLifecycle } from '$lib/auth/lifecycle';
  import { errorMessage as accessErrorMessage } from '$lib/api/http';
  import CursorPagination from '$lib/components/CursorPagination.svelte';

  let selectedUser = $state('');
  let previousSelectedUser = $state('');
  let cursor = $state<string | undefined>();
  let history = $state<Array<string | undefined>>([]);
  let busy = $state('');
  let error = $state('');
  let notice = $state('');

  const users = createQuery(() => ({
    queryKey: ['user-page', 'first'],
    queryFn: () => listUserPage()
  }));
  const sessions = createQuery(() => ({
    queryKey: ['session-page', selectedUser, cursor ?? 'first'],
    queryFn: () => listSessionPage(selectedUser || undefined, cursor)
  }));

  $effect(() => {
    if (selectedUser === previousSelectedUser) return;
    previousSelectedUser = selectedUser;
    cursor = undefined;
    history = [];
  });

  async function removeSession(id: string, current: boolean) {
    if (
      !confirm(
        current ? 'Sign out this current session?' : 'Revoke this session?'
      )
    )
      return;
    busy = `session-${id}`;
    error = notice = '';
    try {
      if (current) {
        await authLifecycle.endCurrentSession((signal) => logout(signal));
        return;
      }
      await revokeSession(id);
      await sessions.refetch();
      notice = 'Session revoked.';
    } catch (cause) {
      error = accessErrorMessage(cause);
    } finally {
      busy = '';
    }
  }

  function nextPage() {
    const next = sessions.data?.nextCursor;
    if (!next) return;
    history = [...history, cursor];
    cursor = next;
  }

  function previousPage() {
    cursor = history.at(-1);
    history = history.slice(0, -1);
  }
</script>

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

<div class="toolbar">
  <div>
    <p class="eyebrow">Active sessions</p>
    <h2>Opaque server-side sessions</h2>
  </div>
  <label class="session-filter">
    <span>Member</span>
    <select bind:value={selectedUser}>
      <option value="">My sessions</option>
      {#each users.data?.items ?? [] as user (user.id)}<option value={user.id}
          >{user.display_name}</option
        >{/each}
    </select>
  </label>
</div>

{#if sessions.isPending}
  <div class="loading-state" role="status">Loading sessions…</div>
{:else if sessions.isError}
  <div class="inline-problem" role="alert">
    {accessErrorMessage(sessions.error)}
  </div>
{:else if !sessions.data?.items.length && history.length === 0}
  <section class="card empty-state">
    <p>No active sessions in this view.</p>
  </section>
{:else}
  <div class="table-shell">
    <table class="data-table">
      <thead
        ><tr
          ><th>Session ID</th><th>Status</th><th>Created</th><th>Last seen</th
          ><th>Expires</th><th><span class="sr-only">Actions</span></th></tr
        ></thead
      >
      <tbody>
        {#each sessions.data?.items ?? [] as session (session.id)}
          <tr>
            <td><code>{session.id}</code></td><td
              ><span class:accent={session.current} class="badge"
                >{session.current ? 'current' : 'active'}</span
              ></td
            >
            <td>{new Date(session.created_at).toLocaleString()}</td><td
              >{new Date(session.last_seen_at).toLocaleString()}</td
            ><td>{new Date(session.expires_at).toLocaleString()}</td>
            <td
              ><button
                class="button button-secondary danger-button"
                type="button"
                onclick={() => removeSession(session.id, session.current)}
                disabled={Boolean(busy)}
                >{busy === `session-${session.id}`
                  ? 'Revoking…'
                  : session.current
                    ? 'Sign out'
                    : 'Revoke'}</button
              ></td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <CursorPagination
    page={history.length + 1}
    hasPrevious={history.length > 0}
    hasNext={Boolean(sessions.data?.nextCursor)}
    onPrevious={previousPage}
    onNext={nextPage}
    label="Session pages"
  />
{/if}

<style>
  .success-banner {
    margin: 1rem 0;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }
  h2 {
    margin: 0;
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }
  .session-filter {
    display: grid;
    gap: 0.3rem;
    color: var(--foreground-muted);
    font-size: 0.72rem;
    font-weight: 700;
  }
  .session-filter select {
    min-height: 2.5rem;
    padding: 0.5rem 0.65rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .danger-button {
    color: var(--danger);
  }
  code {
    font:
      0.72rem 'JetBrains Mono Variable',
      monospace;
  }
</style>
