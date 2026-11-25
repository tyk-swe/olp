<script lang="ts">
  import { notify } from '#lib/components/toasts.svelte.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import { confirmAction } from '#lib/components/confirm.js';
  import { profileKeys } from '#lib/features/identity/profile/profileKeys.js';

  import { createQuery } from '@tanstack/svelte-query';
  import { logout } from '#lib/session/api.js';
  import {
    listSessionPage,
    revokeSession
  } from '#lib/features/identity/sessions/api.js';
  import { authLifecycle } from '#lib/session/lifecycle.js';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import { formatDate } from '#lib/format.js';

  const pagination = $state(emptyCursorHistory());
  const revoke = useAction();
  const exit = useAction(undefined, 'authentication');

  const sessions = createQuery(() => ({
    queryKey: profileKeys.sessions(pagination.cursor),
    queryFn: () => listSessionPage(undefined, pagination.cursor)
  }));

  async function endSession(id: string, current: boolean) {
    if (revoke.busy || exit.busy) return;
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
        if (!active()) return;
        notify('Session revoked.');
        await sessions.refetch();
      },
      { key: id, fallback: 'The session could not be revoked.' }
    );
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

  {#if revoke.error || exit.error}<div class="inline-problem" role="alert">
      {revoke.error || exit.error}
    </div>{/if}
  <QueryView
    query={sessions}
    loading="Loading sessions…"
    failure={errorMessage(sessions.error, 'Sessions are unavailable.')}
    retryLabel="Try again"
  >
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
              Last session verification {formatDate(session.last_seen_at)} · Expires
              {formatDate(session.expires_at)}
            </p>
            <small
              >{session.browser_hint || 'Unknown browser'} · Created {formatDate(
                session.created_at
              )}</small
            >
          </div>
          <button
            class="button button-secondary"
            type="button"
            onclick={() => endSession(session.id, session.current)}
            disabled={revoke.busy || exit.busy}
            >{revoke.key === session.id || exit.key === session.id
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
  </QueryView>
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
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
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
    color: var(--signal);
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
