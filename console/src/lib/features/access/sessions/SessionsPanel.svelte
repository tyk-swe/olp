<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { userKeys } from '#lib/features/access/users/userKeys.js';

  import { createQuery } from '@tanstack/svelte-query';
  import { logout } from '#lib/session/api.js';
  import {
    listSessionPage,
    revokeSession
  } from '#lib/features/access/sessions/api.js';
  import { listUsers } from '#lib/features/access/users/api.js';
  import { authLifecycle } from '#lib/session/lifecycle.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import { formatDate } from '#lib/format.js';

  const access = useRole();
  const canManage = $derived(access.can('sessions.manage'));
  let selectedUser = $state('');
  const pagination = $state(emptyCursorHistory());
  const revoke = useAction();
  const exit = useAction(undefined, 'authentication');

  const users = createQuery(() => ({
    queryKey: userKeys.roster,
    queryFn: ({ signal }) => listUsers(signal),
    // Only the member filter needs the roster, and only managers see it.
    enabled: canManage
  }));
  const sessions = createQuery(() => ({
    queryKey: userKeys.sessions(selectedUser, pagination.cursor),
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
      !(await confirmAction({
        title: current ? 'Sign out here?' : 'Sign out that session?',
        body: current
          ? 'You’ll go back to the sign-in page.'
          : 'Whoever is using it is signed out right away.',
        confirmLabel: 'Sign out',
        danger: true
      }))
    )
      return;
    if (current) {
      await exit.run(
        () => authLifecycle.endCurrentSession((signal) => logout(signal)),
        undefined,
        { key: id }
      );
      return;
    }
    await revoke.run(
      () => revokeSession(id),
      async (_value, active) => {
        await sessions.refetch();
        if (active()) notify('Session revoked.');
      },
      { key: id, fallback: 'The session could not be revoked.' }
    );
  }
</script>

{#if revoke.error || exit.error}<div class="inline-problem" role="alert">
    {revoke.error || exit.error}
  </div>{/if}

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
        {#each users.data ?? [] as user (user.id)}<option value={user.id}
            >{user.display_name}</option
          >{/each}
      </select>
    </label>
  {/if}
</div>

<QueryView
  query={sessions}
  loading="Loading sessions…"
  emptyWhen={!sessions.data?.items.length && pagination.history.length === 0}
>
  {#snippet empty()}
    <EmptyState title="No active sessions"
      >Nobody is signed in that matches this view.</EmptyState
    >
  {/snippet}
  <ScrollTable label="Sessions">
    <table class="data-table">
      <thead
        ><tr
          ><th>Browser / device</th><th>Status</th><th>Created</th><th
            >Last session verification</th
          ><th>Expires</th><th><span class="sr-only">Actions</span></th></tr
        ></thead
      >
      <tbody>
        {#each sessions.data?.items ?? [] as session (session.id)}
          <tr>
            <td>{session.browser_hint || 'Unknown browser'}</td><td
              ><span class:accent={session.current} class="badge"
                >{session.current ? 'current' : 'active'}</span
              ></td
            >
            <td>{formatDate(session.created_at)}</td><td
              >{formatDate(session.last_seen_at)}</td
            ><td>{formatDate(session.expires_at)}</td>
            <td
              >{#if canManage || session.current}<button
                  class="button button-secondary button-danger"
                  type="button"
                  onclick={() => removeSession(session.id, session.current)}
                  disabled={revoke.busy || exit.busy}
                  >{revoke.key === session.id || exit.key === session.id
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
  </ScrollTable>
  <CursorPagination
    {...cursorPaginationProps(pagination, sessions.data?.nextCursor)}
    label="Session pages"
  />
</QueryView>

<style>
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .session-filter {
    display: grid;
    gap: 0.4rem;
    font-weight: 500;
  }
  .session-filter select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-weight: 400;
    transition: border-color var(--motion);
  }
  .session-filter select:hover {
    border-color: var(--border-strong);
  }
</style>
