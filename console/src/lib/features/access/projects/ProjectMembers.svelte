<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import Callout from '#lib/components/Callout.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import {
    putProjectMember,
    removeProjectMember,
    type ProjectRole
  } from './api';
  import {
    listProjectMemberPage,
    type Project,
    type ProjectMember
  } from '#lib/domain/projects/api.js';
  import { projectKeys } from '#lib/domain/projects/projectKeys.js';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { userQueries } from '#lib/domain/users/queries.js';

  let { project }: { project: Project } = $props();

  const queryClient = useQueryClient();
  const pagination = $state(emptyCursorHistory());
  const members = createQuery(() => ({
    queryKey: projectKeys.members(project.id, pagination.cursor),
    queryFn: ({ signal }) =>
      listProjectMemberPage(project.id, pagination.cursor, signal)
  }));
  const allMembers = createQuery(() => projectQueries.allMembers(project.id));
  const users = createQuery(userQueries.all);
  const memberIds = $derived(
    new Set((allMembers.data ?? []).map((member) => member.user_id))
  );
  const candidates = $derived(
    allMembers.data
      ? (users.data ?? []).filter((user) => !memberIds.has(user.id))
      : []
  );

  const action = useAction();
  let addUserId = $state('');
  let addRole = $state<ProjectRole>('viewer');

  const ROLE_LABELS: Record<ProjectRole, string> = {
    manager: 'Manager',
    viewer: 'Viewer'
  };

  async function run(key: string, send: () => Promise<unknown>, done: string) {
    await action.run(
      send,
      async () => {
        notify(done);
        await queryClient.invalidateQueries({ queryKey: projectKeys.root });
      },
      {
        key,
        failure: (cause) =>
          isEtagMismatch(cause)
            ? 'Someone else changed this project. Reload the page and try again.'
            : errorMessage(cause)
      }
    );
  }

  async function add(event: SubmitEvent) {
    event.preventDefault();
    if (action.busy || !addUserId || memberIds.has(addUserId)) return;
    await run(
      'add',
      () => putProjectMember(project, addUserId, addRole),
      'Member added'
    );
    addUserId = '';
  }

  async function changeRole(member: ProjectMember, select: HTMLSelectElement) {
    const role = select.value as ProjectRole;
    if (role === member.project_role) return;
    const confirmed = await confirmAction({
      title: `Make ${member.display_name || member.email} a ${ROLE_LABELS[role].toLowerCase()}?`,
      body:
        role === 'manager'
          ? 'Managers can change this project’s keys, routes and limits.'
          : 'Viewers can see this project but can’t change it.',
      confirmLabel: `Make ${ROLE_LABELS[role].toLowerCase()}`
    });
    if (!confirmed) {
      select.value = member.project_role;
      return;
    }
    await run(
      `role:${member.user_id}`,
      () => putProjectMember(project, member.user_id, role),
      'Role updated'
    );
  }

  async function remove(member: ProjectMember) {
    const confirmed = await confirmAction({
      title: `Remove ${member.display_name || member.email}?`,
      body: `They lose access to ${project.name} right away.`,
      confirmLabel: 'Remove',
      danger: true
    });
    if (!confirmed) return;
    await run(
      `remove:${member.user_id}`,
      () => removeProjectMember(project, member.user_id),
      'Member removed'
    );
  }
</script>

{#if action.error}<Callout tone="danger">{action.error}</Callout>{/if}

{#if members.isPending}
  <div class="loading-state" role="status">Loading members…</div>
{:else if members.isError}
  <Callout tone="danger" title="Members didn’t load">
    {errorMessage(members.error)}
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={() => members.refetch()}
      >
        Try again
      </button>{/snippet}
  </Callout>
{:else}
  <ScrollTable label="Project members">
    <table class="data-table">
      <thead>
        <tr>
          <th>Name</th><th>Email</th><th>Role</th><th>
            <span class="sr-only">Actions</span>
          </th>
        </tr>
      </thead>
      <tbody>
        {#each members.data.items as member (member.user_id)}
          <tr>
            <td>{member.display_name}</td>
            <td>{member.email}</td>
            <td>
              <select
                aria-label="Role for {member.email}"
                value={member.project_role}
                disabled={action.busy}
                onchange={(event) => changeRole(member, event.currentTarget)}
              >
                <option value="manager">Manager</option><option value="viewer">
                  Viewer
                </option>
              </select>
            </td>
            <td class="row-actions">
              <button
                class="text-button danger"
                type="button"
                disabled={action.busy}
                onclick={() => remove(member)}
              >
                {action.key === `remove:${member.user_id}`
                  ? 'Removing…'
                  : 'Remove'}
              </button>
            </td>
          </tr>
        {:else}
          <tr><td colspan="4" class="empty">No members yet</td></tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
  <CursorPagination
    label="Member pages"
    {...cursorPaginationProps(pagination, members.data.nextCursor)}
  />
{/if}

<form class="add" aria-label="Add a member" onsubmit={add}>
  <div class="form-field">
    <label for="member-user">Person</label><select
      id="member-user"
      bind:value={addUserId}
      disabled={action.busy || !allMembers.data}
      required
    >
      <option value="" disabled>
        {candidates.length ? 'Choose someone' : 'Everyone is already a member'}
      </option>{#each candidates as candidate (candidate.id)}<option
          value={candidate.id}
        >
          {candidate.display_name || candidate.email}
        </option>{/each}
    </select>
  </div>
  <div class="form-field">
    <label for="member-role">Role</label><select
      id="member-role"
      bind:value={addRole}
      disabled={action.busy}
    >
      <option value="viewer">Viewer</option><option value="manager">
        Manager
      </option>
    </select>
  </div>
  <button
    class="button button-secondary"
    type="submit"
    disabled={action.busy || !addUserId}
  >
    {action.key === 'add' ? 'Adding…' : 'Add member'}
  </button>
</form>

<style>
  .add {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.75rem;
  }
  .add .form-field {
    min-width: 14rem;
  }
  .empty {
    color: var(--foreground-muted);
  }
</style>
