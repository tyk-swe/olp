<script lang="ts">
  import { userKeys } from '$lib/features/access/users/userKeys';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    listUserPage,
    updateUser,
    type User
  } from '$lib/features/access/users/api';
  import type { CursorPage } from '$lib/api/http';
  import { errorMessage } from '$lib/api/http';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/lists/pagination';
  import { FIXED_ROLES } from '$lib/features/access/session/authorization';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { formatDate } from '$lib/format';
  import { updateMembers } from './bulk';

  const queryClient = useQueryClient();
  const access = useRole();
  const viewer = $derived(access.user);
  const canManage = $derived(access.can('users.manage'));
  const pagination = $state(emptyCursorHistory());
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
  let selected = $state<string[]>([]);
  let bulkAction = $state('deactivate');
  $effect(() => {
    void pagination.cursor;
    selected = [];
  });

  const users = createQuery(() => ({
    queryKey: userKeys.page(pagination.cursor),
    queryFn: () => listUserPage(pagination.cursor)
  }));

  async function run(label: string, action: () => Promise<void>) {
    busy = label;
    error = notice = '';
    try {
      await action();
      return true;
    } catch (cause) {
      error = errorMessage(cause);
      return false;
    } finally {
      busy = '';
    }
  }

  /** Role changes and deactivation update the user and revoke sessions. */
  async function refreshUserViews() {
    await queryClient.invalidateQueries({
      queryKey: userKeys.root
    });
  }

  function updateCachedUser(updated: User) {
    queryClient.setQueryData<CursorPage<User>>(
      userKeys.page(pagination.cursor),
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

  async function changeRole(user: User, select: HTMLSelectElement) {
    const role = select.value;
    if (role === user.role) return;
    const saved = await run(`role-${user.id}`, async () => {
      const updated = await updateUser(user, { role });
      updateCachedUser(updated);
      await refreshUserViews();
      notice = `${updated.display_name} is now ${updated.role}. Existing sessions were revoked.`;
    });
    if (!saved) select.value = user.role;
  }

  async function applyBulk() {
    const members = (users.data?.items ?? []).filter(
      (user) => selected.includes(user.id) && user.id !== viewer?.id
    );
    if (!members.length || !canManage || busy) return;
    if (
      bulkAction === 'deactivate' &&
      !window.confirm(
        `Deactivate ${members.length} members and revoke their sessions? Review their API keys separately.`
      )
    )
      return;
    await run('bulk', async () => {
      const patch = bulkAction.startsWith('role:')
        ? { role: bulkAction.slice(5) as User['role'] }
        : { active: bulkAction === 'reactivate' };
      const result = await updateMembers(members, patch);
      result.updated.forEach(updateCachedUser);
      selected = result.failed.map(({ user }) => user.id);
      await refreshUserViews();
      notice = `${result.updated.length} of ${members.length} members updated. Existing sessions were revoked; attributed API keys remain active.`;
      error = result.failed
        .map(({ user, message }) => `${user.display_name}: ${message}`)
        .join(' ');
    });
  }

  async function changeScope(user: User, select: HTMLSelectElement) {
    const access_scope = select.value as 'global' | 'assigned';
    if (access_scope === user.access_scope) return;
    const saved = await run(`scope-${user.id}`, async () => {
      const updated = await updateUser(user, { access_scope });
      updateCachedUser(updated);
      await refreshUserViews();
      notice = `${updated.display_name} now has ${updated.access_scope === 'assigned' ? 'project-scoped' : 'installation-wide'} access. Existing sessions were revoked.`;
    });
    if (!saved) select.value = user.access_scope;
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
      const updated = await updateUser(user, { active });
      updateCachedUser(updated);
      await refreshUserViews();
      notice = active
        ? `${updated.display_name} can sign in again.`
        : `${updated.display_name} was deactivated and existing sessions were revoked. Next: review API Keys for keys attributed to this member; installation-scoped keys are not automatically revoked.`;
    });
  }
</script>

{#if !canManage}
  <ReadOnlyNote>
    Your role can view members but not change roles or deactivate accounts.
  </ReadOnlyNote>
{/if}
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
    {errorMessage(users.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => users.refetch()}>Retry</button
    >
  </div>
{:else}
  {#if canManage}<div class="bulk-actions">
      <span>{selected.length} selected on this page</span>
      <button
        class="button button-secondary"
        type="button"
        disabled={Boolean(busy) || users.isFetching}
        onclick={() => users.refetch()}>Refresh members</button
      >
      <label
        ><span class="sr-only">Bulk member action</span><select
          bind:value={bulkAction}
          class="filter-control"
          disabled={Boolean(busy)}
          ><option value="deactivate">Deactivate</option><option
            value="reactivate">Reactivate</option
          >{#each FIXED_ROLES as role (role)}<option value={`role:${role}`}
              >Set role: {role}</option
            >{/each}</select
        ></label
      >
      <button
        class="button button-secondary"
        type="button"
        disabled={!selected.length || Boolean(busy)}
        onclick={applyBulk}
        >{busy === 'bulk'
          ? 'Updating members…'
          : 'Apply to selected members'}</button
      >
      <button
        class="button button-quiet"
        type="button"
        disabled={!selected.length || Boolean(busy)}
        onclick={() => (selected = [])}>Clear selection</button
      >
    </div>{/if}
  <div class="table-shell">
    <table class="data-table">
      <thead
        ><tr
          >{#if canManage}<th
              ><input
                type="checkbox"
                aria-label="Select all members on this page"
                disabled={Boolean(busy)}
                checked={Boolean(
                  users.data?.items.filter((user) => user.id !== viewer?.id)
                    .length
                ) &&
                  users.data?.items
                    .filter((user) => user.id !== viewer?.id)
                    .every((user) => selected.includes(user.id))}
                onchange={(event) =>
                  (selected = event.currentTarget.checked
                    ? (users.data?.items ?? [])
                        .filter((user) => user.id !== viewer?.id)
                        .map((user) => user.id)
                    : [])}
              /></th
            >{/if}<th>Member</th><th>Status</th><th>Fixed role</th><th
            >Access scope</th
          ><th>Joined</th><th><span class="sr-only">Actions</span></th></tr
        ></thead
      >
      <tbody>
        {#each users.data?.items ?? [] as user (user.id)}
          <tr>
            {#if canManage}<td
                ><input
                  type="checkbox"
                  aria-label={`Select ${user.display_name}`}
                  value={user.id}
                  bind:group={selected}
                  disabled={user.id === viewer?.id || Boolean(busy)}
                /></td
              >{/if}
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
                  onchange={(event) => changeRole(user, event.currentTarget)}
                  disabled={!canManage ||
                    !user.active ||
                    user.id === viewer?.id ||
                    Boolean(busy)}
                >
                  {#each FIXED_ROLES as role (role)}<option value={role}
                      >{role}</option
                    >{/each}
                </select>
              </label>
            </td>
            <td>
              <label>
                <span class="sr-only">Access scope for {user.display_name}</span
                >
                <select
                  class="role-select"
                  value={user.access_scope}
                  onchange={(event) => changeScope(user, event.currentTarget)}
                  disabled={!canManage ||
                    !user.active ||
                    user.id === viewer?.id ||
                    Boolean(busy)}
                >
                  <option value="global">Installation-wide</option>
                  <option value="assigned">Assigned projects</option>
                </select>
              </label>
            </td>
            <td>{formatDate(user.created_at)}</td>
            <td
              >{#if user.id === viewer?.id}<small>Your account</small
                >{:else if canManage}<button
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
                >{/if}
              <a
                class="button button-secondary"
                href={`${resolve('/api-keys')}?created_by=${user.id}`}
                aria-label={`Review API keys issued by ${user.display_name}`}
                >Review API keys</a
              ></td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <CursorPagination
    {...cursorPaginationProps(pagination, users.data?.nextCursor)}
    hasPrevious={!busy && pagination.history.length > 0}
    hasNext={!busy && Boolean(users.data?.nextCursor)}
    label="Member pages"
  />
{/if}

<style>
  .bulk-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
    align-items: center;
    margin-block: 1rem;
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
    padding: 1rem;
    border: 1px solid var(--border-hairline);
    border-radius: var(--radius-card);
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
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    transition: border-color var(--motion);
  }
  .role-select:hover:not(:disabled) {
    border-color: var(--border-strong);
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
