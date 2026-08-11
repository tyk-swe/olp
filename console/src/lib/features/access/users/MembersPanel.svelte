<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    listUserPage,
    updateUserActive,
    updateUserRole,
    type User
  } from '$lib/api/management/access';
  import type { CursorPage } from '$lib/api/management/shared';
  import { errorMessage as accessErrorMessage } from '$lib/api/http';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { FIXED_ROLES } from '../shared';

  const queryClient = useQueryClient();
  let cursor = $state<string | undefined>();
  let history = $state<Array<string | undefined>>([]);
  let busy = $state('');
  let error = $state('');
  let notice = $state('');

  const users = createQuery(() => ({
    queryKey: ['user-page', cursor ?? 'first'],
    queryFn: () => listUserPage(cursor)
  }));

  async function run(label: string, action: () => Promise<void>) {
    busy = label;
    error = notice = '';
    try {
      await action();
    } catch (cause) {
      error = accessErrorMessage(cause);
    } finally {
      busy = '';
    }
  }

  function updateCachedUser(updated: User) {
    queryClient.setQueryData<CursorPage<User>>(
      ['user-page', cursor ?? 'first'],
      (current) =>
        current
          ? {
              ...current,
              items: current.items.map((item) =>
                item.id === updated.id ? updated : item
              )
            }
          : current
    );
  }

  async function changeRole(user: User, role: string) {
    if (role === user.role) return;
    await run(`role-${user.id}`, async () => {
      const updated = await updateUserRole(user, role);
      updateCachedUser(updated);
      notice = `${updated.display_name} is now ${updated.role}. Existing sessions were revoked.`;
    });
  }

  async function changeActive(user: User) {
    const active = !user.active;
    if (
      !active &&
      !confirm(
        `Deactivate ${user.display_name}? Every active session will be revoked. API keys are installation-scoped and will remain active; after deactivation, review the API-key inventory and explicitly rotate or revoke any keys attributed to this member.`
      )
    )
      return;

    await run(`active-${user.id}`, async () => {
      const updated = await updateUserActive(user, active);
      updateCachedUser(updated);
      notice = active
        ? `${updated.display_name} can sign in again.`
        : `${updated.display_name} was deactivated and existing sessions were revoked. Next: review API Keys for keys attributed to this member; installation-scoped keys are not automatically revoked.`;
    });
  }

  function nextPage() {
    const next = users.data?.nextCursor;
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

<div class="role-guide" aria-label="Fixed role permissions">
  {#each [['owner', 'Full control, identity, and access'], ['operator', 'Gateway configuration and operations'], ['developer', 'Keys, playground, and request metadata'], ['viewer', 'Read-only monitoring']] as role (role[0])}
    <div>
      <span class="badge accent">{role[0]}</span><small>{role[1]}</small>
    </div>
  {/each}
</div>

{#if users.isPending}
  <div class="loading-state" role="status">Loading members…</div>
{:else if users.isError}
  <div class="inline-problem" role="alert">
    {accessErrorMessage(users.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => users.refetch()}>Retry</button
    >
  </div>
{:else}
  <div class="table-shell">
    <table class="data-table">
      <thead
        ><tr
          ><th>Member</th><th>Status</th><th>Fixed role</th><th>Joined</th><th
            ><span class="sr-only">Actions</span></th
          ></tr
        ></thead
      >
      <tbody>
        {#each users.data?.items ?? [] as user (user.id)}
          <tr>
            <td
              ><strong>{user.display_name}</strong><br /><span
                >{user.email}</span
              ></td
            >
            <td
              ><span
                class:success={user.active}
                class:danger={!user.active}
                class="badge">{user.active ? 'active' : 'disabled'}</span
              ></td
            >
            <td>
              <label>
                <span class="sr-only">Role for {user.display_name}</span>
                <select
                  class="role-select"
                  value={user.role}
                  onchange={(event) =>
                    changeRole(user, event.currentTarget.value)}
                  disabled={!user.active || busy === `role-${user.id}`}
                >
                  {#each FIXED_ROLES as role (role)}<option value={role}
                      >{role}</option
                    >{/each}
                </select>
              </label>
            </td>
            <td>{new Date(user.created_at).toLocaleDateString()}</td>
            <td
              ><button
                class="button button-secondary"
                class:danger-button={user.active}
                type="button"
                onclick={() => changeActive(user)}
                disabled={Boolean(busy)}
                >{busy === `active-${user.id}`
                  ? 'Saving…'
                  : user.active
                    ? 'Deactivate'
                    : 'Reactivate'}</button
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
    hasNext={Boolean(users.data?.nextCursor)}
    onPrevious={previousPage}
    onNext={nextPage}
    label="Member pages"
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
  .role-guide {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: 0.65rem;
    margin-bottom: 1rem;
  }
  .role-guide div {
    display: grid;
    align-content: start;
    gap: 0.45rem;
    min-height: 5.5rem;
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface);
  }
  .role-guide .badge {
    justify-self: start;
  }
  .role-guide small,
  td span {
    color: var(--foreground-muted);
  }
  .role-select {
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
  @media (max-width: 64rem) {
    .role-guide {
      grid-template-columns: repeat(2, 1fr);
    }
  }
  @media (max-width: 42rem) {
    .role-guide {
      grid-template-columns: 1fr;
    }
  }
</style>
