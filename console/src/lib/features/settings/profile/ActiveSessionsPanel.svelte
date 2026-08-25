<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { logout } from '$lib/api/auth';
  import { listSessionPage, revokeSession } from '$lib/api/management/access';
  import { errorMessage } from '$lib/api/http';
  import { authLifecycle } from '$lib/auth/lifecycle';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/api/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { formatDate } from '$lib/format';

  const pagination = $state(emptyCursorHistory());
  let revoking = $state('');
  let error = $state('');
  let notice = $state('');

  const sessions = createQuery(() => ({
    queryKey: ['profile-sessions', pagination.cursor],
    queryFn: () => listSessionPage(undefined, pagination.cursor)
  }));

  async function endSession(id: string, current: boolean) {
    if (
      !confirm(current ? 'Sign out of this session?' : 'Revoke this session?')
    )
      return;
    revoking = id;
    error = notice = '';
    try {
      if (current) {
        await authLifecycle.endCurrentSession((signal) => logout(signal));
        return;
      }
      await revokeSession(id);
      notice = 'Session revoked.';
      await sessions.refetch();
    } catch (cause) {
      error = errorMessage(cause, 'The session could not be revoked.');
    } finally {
      revoking = '';
    }
  }
</script>

<section class="sessions" aria-labelledby="sessions-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Security</p>
      <h2 id="sessions-title">Active sessions</h2>
      <p>Review signed-in browsers and revoke anything you do not recognize.</p>
    </div>
    <button
      class="button button-secondary"
      type="button"
      onclick={() => sessions.refetch()}
      disabled={sessions.isFetching}>Refresh</button
    >
  </div>
  {#if notice}<p class="success-message" role="status">{notice}</p>{/if}
  {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
  {#if sessions.isPending}
    <div class="loading-state" role="status">Loading sessions…</div>
  {:else if sessions.isError}
    <div class="inline-problem" role="alert">
      {errorMessage(sessions.error, 'Sessions are unavailable.')}
      <button
        class="text-button"
        type="button"
        onclick={() => sessions.refetch()}>Try again</button
      >
    </div>
  {:else}
    <div class="session-list">
      {#each sessions.data?.items ?? [] as session (session.id)}
        <article class="card session-row">
          <div class="session-icon" aria-hidden="true">
            {session.current ? '●' : '○'}
          </div>
          <div>
            <div class="session-heading">
              <strong
                >{session.current
                  ? 'Current session'
                  : 'Browser session'}</strong
              >
              {#if session.current}<span class="badge success">This device</span
                >{/if}
            </div>
            <p>
              Last active {formatDate(session.last_seen_at)} · Expires {formatDate(
                session.expires_at
              )}
            </p>
            <small class="mono">{session.id}</small>
          </div>
          <button
            class="button button-secondary"
            type="button"
            onclick={() => endSession(session.id, session.current)}
            disabled={revoking === session.id}
            >{revoking === session.id
              ? 'Revoking…'
              : session.current
                ? 'Sign out'
                : 'Revoke'}</button
          >
        </article>
      {/each}
    </div>
    {#if pagination.history.length > 0 || sessions.data?.nextCursor}
      <CursorPagination
        {...cursorPaginationProps(pagination, sessions.data?.nextCursor)}
        label="Session pages"
      />
    {/if}
  {/if}
</section>

<style>
  .sessions {
    margin-top: 2rem;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
    margin-bottom: 0.8rem;
  }
  .section-heading p:last-child {
    margin: 0.3rem 0 0;
    color: var(--foreground-muted);
  }
  .session-list {
    display: grid;
    gap: 0.7rem;
  }
  .session-row {
    display: grid;
    grid-template-columns: auto 1fr auto;
    align-items: center;
    gap: 0.85rem;
    padding: 1rem;
  }
  .session-icon {
    color: var(--success);
  }
  .session-heading {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .session-row p,
  .session-row small {
    display: block;
    margin: 0.2rem 0 0;
    color: var(--foreground-muted);
    overflow-wrap: anywhere;
  }
  .text-button {
    min-height: 2.75rem;
    border: 0;
    background: transparent;
    color: var(--accent-strong);
    font-weight: 700;
  }
  .success-message {
    margin: 0 0 1rem;
    padding: 0.8rem 1rem;
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
    font-weight: 700;
  }
  @media (max-width: 40rem) {
    .session-row {
      grid-template-columns: auto 1fr;
    }
    .session-row .button {
      grid-column: 1 / -1;
    }
    .section-heading {
      display: grid;
    }
  }
</style>
