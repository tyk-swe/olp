<script lang="ts">
  import { userKeys } from '#lib/features/access/users/userKeys.js';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    listUserPage,
    updateUser,
    type User
  } from '#lib/features/access/users/api.js';
  import type { CursorPage } from '#lib/management/http.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import { FIXED_ROLES } from '#lib/features/access/session/authorization.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import Menu from '#lib/components/Menu.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { formatDay } from '#lib/format.js';
  import { updateMembers } from './bulk';
  import MemberAccessDialog from './MemberAccessDialog.svelte';
  import { ROLE_DETAILS, SCOPE_DETAILS } from './roles';

  const queryClient = useQueryClient();
  const access = useRole();
  const viewer = $derived(access.user);
  const canManage = $derived(access.can('users.manage'));
  const pagination = $state(emptyCursorHistory());
  let busy = $state('');
  let error = $state('');
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
    error = '';
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

  let editing = $state<User | null>(null);

  async function saveAccess(
    user: User,
    patch: Pick<User, 'role' | 'access_scope'>
  ) {
    return run(`access-${user.id}`, async () => {
      const updated = await updateUser(user, patch);
      updateCachedUser(updated);
      await refreshUserViews();
      notify(
        `${updated.display_name} is now ${ROLE_DETAILS[updated.role].label.toLowerCase()} on ${SCOPE_DETAILS[updated.access_scope].label.toLowerCase()}`
      );
    });
  }

  async function applyBulk() {
    const members = (users.data?.items ?? []).filter(
      (user) => selected.includes(user.id) && user.id !== viewer?.id
    );
    if (!members.length || !canManage || busy) return;
    const role = bulkAction.startsWith('role:')
      ? (bulkAction.slice(5) as User['role'])
      : null;
    const confirmed = await confirmAction(
      role
        ? {
            title: `Make ${members.length} ${members.length === 1 ? 'person' : 'people'} ${ROLE_DETAILS[role].label.toLowerCase()}s?`,
            body: 'They’ll be signed out everywhere and pick up the new role when they sign in again.',
            confirmLabel: 'Change role'
          }
        : bulkAction === 'deactivate'
          ? {
              title: `Deactivate ${members.length} ${members.length === 1 ? 'person' : 'people'}?`,
              body: 'They’re signed out right away and can’t sign in. API keys they created keep working until you revoke them.',
              confirmLabel: 'Deactivate',
              danger: true
            }
          : {
              title: `Reactivate ${members.length} ${members.length === 1 ? 'person' : 'people'}?`,
              body: 'They’ll be able to sign in again with their current role.',
              confirmLabel: 'Reactivate'
            }
    );
    if (!confirmed) return;
    await run('bulk', async () => {
      const patch = role ? { role } : { active: bulkAction === 'reactivate' };
      const result = await updateMembers(members, patch);
      result.updated.forEach(updateCachedUser);
      selected = result.failed.map(({ user }) => user.id);
      await refreshUserViews();
      if (result.updated.length)
        notify(`Updated ${result.updated.length} of ${members.length}`);
      error = result.failed
        .map(({ user, message }) => `${user.display_name}: ${message}`)
        .join(' ');
    });
  }

  async function changeActive(user: User) {
    const active = !user.active;
    const confirmed = await confirmAction(
      active
        ? {
            title: `Reactivate ${user.display_name}?`,
            body: 'They’ll be able to sign in again with their current role.',
            confirmLabel: 'Reactivate'
          }
        : {
            title: `Deactivate ${user.display_name}?`,
            body: 'They’re signed out right away and can’t sign in. API keys they created keep working until you revoke them.',
            confirmLabel: 'Deactivate',
            danger: true
          }
    );
    if (!confirmed) return;

    await run(`active-${user.id}`, async () => {
      const updated = await updateUser(user, { active });
      updateCachedUser(updated);
      await refreshUserViews();
      notify(
        active
          ? `${updated.display_name} can sign in again`
          : `Deactivated ${updated.display_name}`
      );
    });
  }
</script>

{#if !canManage}
  <ReadOnlyNote>
    Your role can see who has access but can’t change it.
  </ReadOnlyNote>
{/if}

{#if error}<Callout tone="danger">{error}</Callout>{/if}

{#if users.isPending}
  <div class="loading-state" role="status">Loading people…</div>
{:else if users.isError}
  <Callout tone="danger" title="People didn’t load">
    {errorMessage(users.error)}
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={() => users.refetch()}>Try again</button
      >{/snippet}
  </Callout>
{:else if !users.data.items.length}
  <EmptyState title="No one here yet">
    Invite people from the Invitations tab.
  </EmptyState>
{:else}
  {#if canManage && selected.length}<div
      class="bulk-actions"
      role="region"
      aria-label="Selected people"
    >
      <strong>{selected.length} selected</strong>
      <label
        ><span class="sr-only">Action for selected people</span><select
          bind:value={bulkAction}
          class="filter-control"
          disabled={Boolean(busy)}
          ><option value="deactivate">Deactivate</option><option
            value="reactivate">Reactivate</option
          >{#each FIXED_ROLES as role (role)}<option value={`role:${role}`}
              >Make {ROLE_DETAILS[role].label.toLowerCase()}</option
            >{/each}</select
        ></label
      >
      <button
        class="button button-primary"
        type="button"
        disabled={Boolean(busy)}
        onclick={applyBulk}>{busy === 'bulk' ? 'Applying…' : 'Apply'}</button
      >
      <button
        class="button button-quiet"
        type="button"
        disabled={Boolean(busy)}
        onclick={() => (selected = [])}>Clear</button
      >
    </div>
  {/if}

  <div class="table-shell">
    <table class="data-table">
      <thead
        ><tr
          >{#if canManage}<th class="check"
              ><input
                type="checkbox"
                aria-label="Select everyone on this page"
                disabled={Boolean(busy)}
                checked={Boolean(
                  users.data.items.filter((user) => user.id !== viewer?.id)
                    .length
                ) &&
                  users.data.items
                    .filter((user) => user.id !== viewer?.id)
                    .every((user) => selected.includes(user.id))}
                onchange={(event) =>
                  (selected = event.currentTarget.checked
                    ? users.data.items
                        .filter((user) => user.id !== viewer?.id)
                        .map((user) => user.id)
                    : [])}
              />
            </th>
          {/if}
          <th>Name</th>
          <th>Role</th>
          <th>Status</th>
          <th>Joined</th>
          <th><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody>
        {#each users.data.items as user (user.id)}
          <tr>
            {#if canManage}<td class="check"
                ><input
                  type="checkbox"
                  aria-label={`Select ${user.display_name}`}
                  value={user.id}
                  bind:group={selected}
                  disabled={user.id === viewer?.id || Boolean(busy)}
                /></td
              >{/if}
            <td
              ><strong>{user.display_name}</strong
              >{#if user.id === viewer?.id}<span class="you">You</span>{/if}<br
              /><span class="muted">{user.email}</span></td
            >
            <td
              >{ROLE_DETAILS[user.role].label}<br /><span class="muted"
                >{SCOPE_DETAILS[user.access_scope].label}</span
              ></td
            >
            <td
              ><StatusBadge
                label={user.active ? 'Active' : 'Deactivated'}
                tone={user.active ? 'success' : 'danger'}
              /></td
            >
            <td>{formatDay(user.created_at)}</td>
            <td class="row-actions">
              <Menu label={`Actions for ${user.display_name}`}>
                {#if canManage && user.id !== viewer?.id}
                  {#if user.active}<button
                      type="button"
                      onclick={() => (editing = user)}>Change access</button
                    >{/if}
                  <button
                    type="button"
                    class:danger={user.active}
                    onclick={() => changeActive(user)}
                    >{user.active ? 'Deactivate' : 'Reactivate'}</button
                  >
                  <hr />
                {/if}
                <a href={`${resolve('keys')}?created_by=${user.id}`}
                  >API keys they created</a
                >
              </Menu>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <CursorPagination
    {...cursorPaginationProps(pagination, users.data.nextCursor)}
    hasPrevious={!busy && pagination.history.length > 0}
    hasNext={!busy && Boolean(users.data.nextCursor)}
    label="Member pages"
  />
{/if}

{#if editing}
  {@const user = editing}
  <MemberAccessDialog
    {user}
    onSave={(patch) => saveAccess(user, patch)}
    onClose={() => (editing = null)}
  />
{/if}

<style>
  .bulk-actions {
    position: sticky;
    z-index: 5;
    top: 4rem;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.75rem;
    margin-bottom: 1rem;
    padding: 0.6rem 0.75rem;
    border: 1px solid var(--accent);
    border-radius: var(--radius-card);
    background: var(--surface-raised);
    animation: rise var(--dur-enter) var(--ease-out);
  }
  .check {
    width: 2.5rem;
  }
  .muted {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .you {
    margin-left: 0.5rem;
    color: var(--foreground-subtle);
    font-size: var(--text-caption);
  }
  .row-actions {
    text-align: right;
  }
</style>
