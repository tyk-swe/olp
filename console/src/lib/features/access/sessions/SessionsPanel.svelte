<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { logout } from '$lib/api/auth';
  import {
    listSessionPage,
    listUserPage,
    revokeSession
  } from '$lib/api/management/access';
  import { authLifecycle } from '$lib/auth/lifecycle';
  import { useRole } from '$lib/auth/useRole.svelte';
  import { errorMessage as accessErrorMessage } from '$lib/api/http';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '$lib/api/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { formatDate } from '$lib/format';

  const access = useRole();
  const canManage = $derived(access.can('sessions.manage'));
  let selectedUser = $state('');
  const pagination = $state(emptyCursorHistory());
  let busy = $state('');
  let error = $state('');
  let notice = $state('');

  const users = createQuery(() => ({
    queryKey: ['user-page', 'first'],
    queryFn: () => listUserPage(),
    // Only the member filter needs the roster, and only managers see it.
    enabled: canManage
  }));
  const sessions = createQuery(() => ({
    queryKey: ['session-page', selectedUser, pagination.cursor ?? 'first'],
    queryFn: () => listSessionPage(selectedUser || undefined, pagination.cursor)
  }));

  // Resetting inside the change handler keeps the cursor and the member in one
  // render, so the query never fires with a cursor from the previous member.
  function selectMember(value: string) {
    if (value === selectedUser) return;
    selectedUser = value;
    resetCursor(pagination);
  }

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
</script>

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

<div class="toolbar">
  <div>
    <p class="eyebrow">Active sessions</p>
    <h2>Opaque server-side sessions</h2>
  </div>
  {#if canManage}
    <label class="session-filter">
      <span>Member</span>
      <select
        value={selectedUser}
        onchange={(event) => selectMember(event.currentTarget.value)}
      >
        <option value="">My sessions</option>
        {#each users.data?.items ?? [] as user (user.id)}<option value={user.id}
            >{user.display_name}</option
          >{/each}
      </select>
    </label>
  {/if}
</div>

{#if sessions.isPending}
  <div class="loading-state" role="status">Loading sessions…</div>
{:else if sessions.isError}
  <div class="inline-problem" role="alert">
    {accessErrorMessage(sessions.error)}
  </div>
{:else if !sessions.data?.items.length && pagination.history.length === 0}
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
            <td>{formatDate(session.created_at)}</td><td
              >{formatDate(session.last_seen_at)}</td
            ><td>{formatDate(session.expires_at)}</td>
            <td
              >{#if canManage || session.current}<button
                  class="button button-secondary danger-button"
                  type="button"
                  onclick={() => removeSession(session.id, session.current)}
                  disabled={Boolean(busy)}
                  >{busy === `session-${session.id}`
                    ? 'Revoking…'
                    : session.current
                      ? 'Sign out'
                      : 'Revoke'}</button
                >{/if}</td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <CursorPagination
    {...cursorPaginationProps(pagination, sessions.data?.nextCursor)}
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
