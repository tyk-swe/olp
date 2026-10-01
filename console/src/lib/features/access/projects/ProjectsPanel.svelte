<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '$lib/api/http';
  import {
    createProject,
    listAllProjectMembers,
    listProjectMemberPage,
    listProjectPage,
    putProjectMember,
    removeProjectMember,
    renameProject,
    type Project,
    type ProjectMember,
    type ProjectRole
  } from '$lib/features/access/projects/api';
  import { listUsers } from '$lib/features/access/users/api';
  import { userKeys } from '$lib/features/access/users/userKeys';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { formatDate } from '$lib/format';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '$lib/lists/pagination';

  const queryClient = useQueryClient();

  const pagination = $state(emptyCursorHistory());
  const projects = createQuery(() => ({
    queryKey: projectKeys.page(pagination.cursor),
    queryFn: ({ signal }) => listProjectPage(pagination.cursor, signal)
  }));
  const items = $derived(projects.data?.items ?? []);

  const users = createQuery(() => ({
    queryKey: userKeys.roster,
    queryFn: ({ signal }) => listUsers(signal)
  }));

  let createName = $state('');
  let createBusy = $state(false);
  let createError = $state('');

  async function submitCreate(event: SubmitEvent) {
    event.preventDefault();
    if (createBusy || !createName.trim()) return;
    createBusy = true;
    createError = '';
    try {
      const project = await createProject(createName);
      createName = '';
      await queryClient.invalidateQueries({ queryKey: projectKeys.root });
      open(project);
    } catch (error) {
      createError = errorMessage(error);
    } finally {
      createBusy = false;
    }
  }

  let selectedId = $state('');
  const selected = $derived(items.find((project) => project.id === selectedId));
  const memberPagination = $state(emptyCursorHistory());
  const members = createQuery(() => ({
    queryKey: projectKeys.members(selectedId, memberPagination.cursor),
    enabled: Boolean(selectedId),
    queryFn: ({ signal }) =>
      listProjectMemberPage(selectedId, memberPagination.cursor, signal)
  }));
  const memberItems = $derived(members.data?.items ?? []);
  // The add-member picker must exclude members on every page: adding an
  // existing member is an upsert that silently overwrites their role.
  const allMembers = createQuery(() => ({
    queryKey: projectKeys.allMembers(selectedId),
    enabled: Boolean(selectedId),
    queryFn: ({ signal }) => listAllProjectMembers(selectedId, signal)
  }));
  const memberIds = $derived(
    new Set((allMembers.data ?? []).map((member) => member.user_id))
  );
  const candidates = $derived(
    allMembers.data
      ? (users.data ?? []).filter((user) => !memberIds.has(user.id))
      : []
  );

  let renameValue = $state('');
  let memberBusy = $state('');
  let memberError = $state('');
  let memberNotice = $state('');
  let addUserId = $state('');
  let addRole = $state<ProjectRole>('viewer');

  function open(project: Project) {
    selectedId = project.id;
    resetCursor(memberPagination);
    memberError = '';
    memberNotice = '';
    renameValue = project.name;
  }

  function select(project: Project) {
    if (selectedId === project.id) selectedId = '';
    else open(project);
  }

  async function refreshSelected() {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: projectKeys.root }),
      queryClient.invalidateQueries({ queryKey: projectKeys.membersRoot })
    ]);
  }

  async function submitRename(event: SubmitEvent) {
    event.preventDefault();
    const project = selected;
    if (!project || memberBusy || !renameValue.trim()) return;
    memberBusy = 'rename';
    memberError = memberNotice = '';
    try {
      await renameProject(project, renameValue);
      memberNotice = 'Project renamed.';
      await refreshSelected();
    } catch (error) {
      memberError = isEtagMismatch(error)
        ? 'The project changed elsewhere. Reload and try again.'
        : errorMessage(error);
    } finally {
      memberBusy = '';
    }
  }

  async function submitAddMember(event: SubmitEvent) {
    event.preventDefault();
    const project = selected;
    if (!project || memberBusy || !addUserId || !allMembers.data) return;
    if (memberIds.has(addUserId)) {
      memberError =
        'That user is already a member; change their role in the list.';
      return;
    }
    memberBusy = 'add';
    memberError = memberNotice = '';
    try {
      await putProjectMember(project, addUserId, addRole);
      addUserId = '';
      memberNotice = 'Member saved.';
      await refreshSelected();
    } catch (error) {
      memberError = isEtagMismatch(error)
        ? 'The project changed elsewhere. Reload and try again.'
        : errorMessage(error);
    } finally {
      memberBusy = '';
    }
  }

  async function changeMemberRole(member: ProjectMember, role: ProjectRole) {
    const project = selected;
    if (!project || memberBusy || role === member.project_role) return;
    memberBusy = `role:${member.user_id}`;
    memberError = memberNotice = '';
    try {
      await putProjectMember(project, member.user_id, role);
      memberNotice = 'Member role updated.';
      await refreshSelected();
    } catch (error) {
      memberError = isEtagMismatch(error)
        ? 'The project changed elsewhere. Reload and try again.'
        : errorMessage(error);
    } finally {
      memberBusy = '';
    }
  }

  async function removeMember(member: ProjectMember) {
    const project = selected;
    if (!project || memberBusy) return;
    memberBusy = `remove:${member.user_id}`;
    memberError = memberNotice = '';
    try {
      await removeProjectMember(project, member.user_id);
      memberNotice = 'Member removed.';
      await refreshSelected();
    } catch (error) {
      memberError = isEtagMismatch(error)
        ? 'The project changed elsewhere. Reload and try again.'
        : errorMessage(error);
    } finally {
      memberBusy = '';
    }
  }
</script>

<section class="access-panel projects-panel" aria-label="Projects">
  <div class="panel-heading">
    <div>
      <h2>Projects</h2>
      <p>
        Group providers, routes, and API keys. Assign members as managers or
        viewers.
      </p>
    </div>
  </div>

  <form class="create-form" aria-label="Create project" onsubmit={submitCreate}>
    <div class="form-field">
      <label for="project-name">New project name</label><input
        id="project-name"
        bind:value={createName}
        disabled={createBusy}
        maxlength="100"
        required
      />
    </div>
    <button
      class="button button-primary"
      type="submit"
      disabled={createBusy || !createName.trim()}
      >{createBusy ? 'Creating…' : 'Create project'}</button
    >
  </form>
  {#if createError}<p class="inline-problem" role="alert">{createError}</p>{/if}

  {#if projects.isPending}<p class="inline-status" role="status">
      Loading projects…
    </p>
  {:else if projects.isError}<div class="inline-problem" role="alert">
      Projects are unavailable.
      <button
        class="button button-secondary"
        type="button"
        onclick={() => projects.refetch()}>Retry</button
      >
    </div>
  {:else if !items.length && pagination.history.length === 0}<p class="empty">
      No projects exist. Create one to scope resources and memberships.
    </p>
  {:else}
    <div class="table-scroll">
      <table>
        <thead
          ><tr
            ><th>Name</th><th>Members</th><th>Created by</th><th>Created</th><th
              ><span class="sr-only">Manage</span></th
            ></tr
          ></thead
        >
        <tbody>
          {#each items as project (project.id)}
            <tr class:selected={selectedId === project.id}>
              <td><strong>{project.name}</strong></td>
              <td>{project.member_count}</td>
              <td>{project.created_by_email ?? '—'}</td>
              <td>{formatDate(project.created_at)}</td>
              <td class="row-actions"
                ><button
                  class="text-button"
                  type="button"
                  aria-expanded={selectedId === project.id}
                  aria-controls="project-members"
                  onclick={() => select(project)}
                  >{selectedId === project.id ? 'Close' : 'Members'}</button
                ></td
              >
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <CursorPagination
      label="Project pages"
      {...cursorPaginationProps(
        pagination,
        projects.data?.nextCursor,
        () => (selectedId = '')
      )}
    />
  {/if}

  {#if selected}
    <section
      id="project-members"
      class="member-card"
      aria-label="Members of {selected.name}"
    >
      <form class="rename-form" onsubmit={submitRename}>
        <div class="form-field">
          <label for="project-rename">Rename {selected.name}</label><input
            id="project-rename"
            bind:value={renameValue}
            disabled={Boolean(memberBusy)}
            maxlength="100"
            required
          />
        </div>
        <button
          class="button button-secondary"
          type="submit"
          disabled={Boolean(memberBusy) ||
            !renameValue.trim() ||
            renameValue.trim() === selected.name}
          >{memberBusy === 'rename' ? 'Renaming…' : 'Rename'}</button
        >
      </form>

      {#if memberError}<p class="inline-problem" role="alert">
          {memberError}
        </p>{/if}
      {#if memberNotice}<p class="notice" role="status">{memberNotice}</p>{/if}

      {#if members.isPending}<p class="inline-status" role="status">
          Loading members…
        </p>
      {:else if members.isError}<div class="inline-problem" role="alert">
          Members are unavailable.
          <button
            class="button button-secondary"
            type="button"
            onclick={() => members.refetch()}>Retry</button
          >
        </div>
      {:else}
        <div class="table-scroll">
          <table>
            <thead
              ><tr
                ><th>Member</th><th>Email</th><th>Role</th><th
                  ><span class="sr-only">Remove</span></th
                ></tr
              ></thead
            >
            <tbody>
              {#each memberItems as member (member.user_id)}
                <tr>
                  <td>{member.display_name}</td>
                  <td>{member.email}</td>
                  <td
                    ><select
                      aria-label="Role for {member.email}"
                      value={member.project_role}
                      disabled={Boolean(memberBusy)}
                      onchange={(event) =>
                        changeMemberRole(
                          member,
                          event.currentTarget.value as ProjectRole
                        )}
                      ><option value="manager">Manager</option><option
                        value="viewer">Viewer</option
                      ></select
                    ></td
                  >
                  <td class="row-actions"
                    ><button
                      class="text-button danger"
                      type="button"
                      disabled={Boolean(memberBusy)}
                      onclick={() => removeMember(member)}
                      >{memberBusy === `remove:${member.user_id}`
                        ? 'Removing…'
                        : 'Remove'}</button
                    ></td
                  >
                </tr>
              {:else}
                <tr><td colspan="4">No members.</td></tr>
              {/each}
            </tbody>
          </table>
        </div>
        <CursorPagination
          label="Member pages"
          {...cursorPaginationProps(memberPagination, members.data?.nextCursor)}
        />

        <form class="create-form" onsubmit={submitAddMember}>
          <div class="form-field">
            <label for="member-user">Add member</label><select
              id="member-user"
              bind:value={addUserId}
              disabled={Boolean(memberBusy)}
              required
              ><option value="" disabled>Select a user</option
              >{#each candidates as candidate (candidate.id)}<option
                  value={candidate.id}
                  >{candidate.display_name || candidate.email}</option
                >{/each}</select
            >
          </div>
          <div class="form-field">
            <label for="member-role">Role</label><select
              id="member-role"
              bind:value={addRole}
              disabled={Boolean(memberBusy)}
              ><option value="viewer">Viewer</option><option value="manager"
                >Manager</option
              ></select
            >
          </div>
          <button
            class="button button-primary"
            type="submit"
            disabled={Boolean(memberBusy) || !addUserId || !allMembers.data}
            >{memberBusy === 'add' ? 'Adding…' : 'Add member'}</button
          >
        </form>
      {/if}
    </section>
  {/if}
</section>

<style>
  .projects-panel {
    display: grid;
    gap: 1rem;
  }
  .create-form,
  .rename-form {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.75rem;
  }
  .create-form .form-field,
  .rename-form .form-field {
    min-width: 14rem;
  }
  .member-card {
    display: grid;
    gap: 1rem;
    padding: 1rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
  }
  tr.selected {
    background: var(--surface-raised);
  }
  .notice {
    margin: 0;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .empty {
    color: var(--foreground-muted);
  }
</style>
