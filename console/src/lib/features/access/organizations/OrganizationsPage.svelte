<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '../session/useRole.svelte';
  import { projectKeys } from '../projects/projectKeys';
  import AggregateBudgetPanel from '../budgets/AggregateBudgetPanel.svelte';
  import * as api from './api';
  const access = useRole();
  const client = useQueryClient();
  let selected = $state('');
  let projectId = $state('');
  let name = $state('');
  let newName = $state('');
  let projectName = $state('');
  let renameProject = $state('');
  let userId = $state('');
  let role = $state<'manager' | 'viewer'>('viewer');
  let projectUser = $state('');
  let projectRole = $state<'manager' | 'viewer'>('viewer');
  let busy = $state(false);
  let problem = $state('');
  let notice = $state('');
  const root = [...projectKeys.root, 'organizations'];
  const organizations = createQuery(() => ({
    queryKey: root,
    queryFn: ({ signal }) => api.listOrganizations(signal)
  }));
  const organization = $derived(
    organizations.data?.find((o) => o.id === selected)
  );
  const members = createQuery(() => ({
    queryKey: [...root, selected, 'members'],
    enabled: !!selected,
    queryFn: ({ signal }) => api.organizationMembers(selected, signal)
  }));
  const projects = createQuery(() => ({
    queryKey: [...root, selected, 'projects'],
    enabled: !!selected,
    queryFn: ({ signal }) => api.organizationProjects(selected, signal)
  }));
  const project = $derived(projects.data?.find((p) => p.id === projectId));
  const directMembers = createQuery(() => ({
    queryKey: [...root, selected, projectId, 'members'],
    enabled: !!selected && !!projectId,
    queryFn: ({ signal }) => api.projectMembers(selected, projectId, signal)
  }));
  const canCreate = $derived(access.allows('POST /api/v1/organizations'));
  const editable = $derived(
    access.allows('PATCH /api/v1/organizations/{organization_id}') &&
      (canCreate ||
        !!members.data?.some(
          (m) => m.user_id === access.user?.id && m.role === 'manager'
        ))
  );
  $effect(() => {
    if (
      organizations.data &&
      !organizations.data.some((o) => o.id === selected)
    )
      selected = organizations.data[0]?.id ?? '';
  });
  $effect(() => {
    if (projects.data && !projects.data.some((p) => p.id === projectId))
      projectId = projects.data[0]?.id ?? '';
  });
  async function run(action: () => Promise<unknown>) {
    if (busy) return;
    busy = true;
    problem = '';
    notice = '';
    try {
      await action();
      await client.invalidateQueries({ queryKey: projectKeys.root });
      notice = 'Saved.';
    } catch (e) {
      problem = errorMessage(e);
    } finally {
      busy = false;
    }
  }
</script>

<svelte:head><title>Organizations · OLP</title></svelte:head>
<header>
  <h1>Organizations</h1>
  <p>
    Group projects under shared management and budgets. Installation roles still
    determine which operations each person can perform.
  </p>
</header>
{#if problem}<p class="notice error" role="alert">{problem}</p>{/if}
{#if notice}<p class="notice success" role="status">{notice}</p>{/if}
{#if canCreate}
  <form
    onsubmit={(e) => {
      e.preventDefault();
      void run(async () => {
        const o = await api.createOrganization(
          newName.trim(),
          crypto.randomUUID()
        );
        selected = o.id;
        newName = '';
      });
    }}
  >
    <label class="form-field"
      >New organization name<input
        required
        maxlength="100"
        bind:value={newName}
        disabled={busy}
      /></label
    >
    <button class="button button-primary" disabled={busy}
      >Create organization</button
    >
  </form>
{/if}
{#if organizations.isPending}<p>
    Loading organizations…
  </p>{:else if organizations.isError}<p class="notice error" role="alert">
    {errorMessage(organizations.error)}
  </p>
  <button
    class="button button-secondary"
    onclick={() => organizations.refetch()}>Retry</button
  >{:else if organization}
  <label class="form-field"
    >Organization<select bind:value={selected}
      ><option value="" disabled>Choose an organization</option
      >{#each organizations.data ?? [] as o (o.id)}<option value={o.id}
          >{o.name}</option
        >{/each}</select
    ></label
  >
  <p>
    {editable
      ? 'You can manage this organization.'
      : 'You have read-only access to this organization.'}
  </p>
  {#if editable}<form
      onsubmit={(e) => {
        e.preventDefault();
        void run(async () => {
          await api.renameOrganization(
            selected,
            name.trim(),
            organization.etag
          );
          name = '';
        });
      }}
    >
      <label class="form-field"
        >Rename organization<input
          required
          maxlength="100"
          placeholder={organization.name}
          bind:value={name}
          disabled={busy}
        /></label
      ><button class="button button-secondary" disabled={busy}>Save name</button
      >
    </form>{/if}
  {#key selected}<AggregateBudgetPanel
      organizationId={selected}
      {editable}
    />{/key}
  <section aria-labelledby="organization-members">
    <h2 id="organization-members">Organization members</h2>
    <p>
      Managers inherit management of every contained project. Direct project
      memberships remain independent. Add existing users by their user ID.
    </p>
    {#if members.isError}<p role="alert">{errorMessage(members.error)}</p>{/if}
    {#each members.data ?? [] as m (m.user_id)}<p>
        {m.email} · {m.role} · installation {m.installation_role}
        {#if editable}<button
            class="button button-secondary"
            disabled={busy}
            onclick={() =>
              run(() =>
                api.removeOrganizationMember(
                  selected,
                  m.user_id,
                  organization.etag
                )
              )}>Remove {m.email}</button
          >{/if}
      </p>{/each}
    {#if editable}<form
        onsubmit={(e) => {
          e.preventDefault();
          void run(async () => {
            await api.putOrganizationMember(
              selected,
              userId.trim(),
              role,
              organization.etag
            );
            userId = '';
          });
        }}
      >
        <label class="form-field"
          >Organization member user ID<input
            required
            bind:value={userId}
            disabled={busy}
          /></label
        ><label class="form-field"
          >Organization role<select bind:value={role} disabled={busy}
            ><option value="viewer">Viewer</option><option value="manager"
              >Manager</option
            ></select
          ></label
        ><button class="button button-primary" disabled={busy}
          >Assign organization member</button
        >
      </form>{/if}
  </section>
  <section aria-labelledby="organization-projects">
    <h2 id="organization-projects">Projects</h2>
    {#if projects.isError}<p role="alert">
        {errorMessage(projects.error)}
      </p>{/if}
    {#if editable}<form
        onsubmit={(e) => {
          e.preventDefault();
          void run(async () => {
            const p = await api.createOrganizationProject(
              selected,
              projectName.trim(),
              crypto.randomUUID()
            );
            projectName = '';
            projectId = p.id;
          });
        }}
      >
        <label class="form-field"
          >New project name<input
            required
            maxlength="100"
            bind:value={projectName}
            disabled={busy}
          /></label
        ><button class="button button-primary" disabled={busy}
          >Create organization project</button
        >
      </form>{/if}
    {#if project}<label class="form-field"
        >Organization project<select bind:value={projectId}
          >{#each projects.data ?? [] as p (p.id)}<option value={p.id}
              >{p.name}</option
            >{/each}</select
        ></label
      >
      <p>
        Use Project policies, API keys and Notifications to manage this
        project's controls.
      </p>
      {#if editable}<form
          onsubmit={(e) => {
            e.preventDefault();
            void run(async () => {
              await api.renameProject(
                selected,
                projectId,
                renameProject.trim(),
                project.etag
              );
              renameProject = '';
            });
          }}
        >
          <label class="form-field"
            >Rename project<input
              required
              maxlength="100"
              placeholder={project.name}
              bind:value={renameProject}
              disabled={busy}
            /></label
          ><button class="button button-secondary" disabled={busy}
            >Save project name</button
          >
        </form>{/if}
      <h3>Direct project members</h3>
      <p>Organization memberships above apply in addition to this list.</p>
      {#if directMembers.isError}<p role="alert">
          {errorMessage(directMembers.error)}
        </p>{/if}
      {#each directMembers.data ?? [] as m (m.user_id)}<p>
          {m.email} · {m.project_role}{#if editable}<button
              class="button button-secondary"
              disabled={busy}
              onclick={() =>
                run(() =>
                  api.removeProjectMember(
                    selected,
                    projectId,
                    m.user_id,
                    project.etag
                  )
                )}>Remove project member {m.email}</button
            >{/if}
        </p>{/each}
      {#if editable}<form
          onsubmit={(e) => {
            e.preventDefault();
            void run(async () => {
              await api.putProjectMember(
                selected,
                projectId,
                projectUser.trim(),
                projectRole,
                project.etag
              );
              projectUser = '';
            });
          }}
        >
          <label class="form-field"
            >Project member user ID<input
              required
              bind:value={projectUser}
              disabled={busy}
            /></label
          ><label class="form-field"
            >Project role<select bind:value={projectRole} disabled={busy}
              ><option value="viewer">Viewer</option><option value="manager"
                >Manager</option
              ></select
            ></label
          ><button class="button button-primary" disabled={busy}
            >Assign project member</button
          >
        </form>{/if}
    {:else}<p>No projects in this organization.</p>{/if}
  </section>
{:else}<p>No visible organizations.</p>{/if}

<style>
  form {
    display: flex;
    flex-wrap: wrap;
    gap: 1rem;
    align-items: end;
    margin-block: 1rem;
  }
  section {
    border-top: 1px solid var(--border);
    margin-top: 2rem;
    padding-top: 1rem;
  }
</style>
