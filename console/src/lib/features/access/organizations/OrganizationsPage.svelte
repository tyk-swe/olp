<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { useMutationIntents } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import DirectProjectMembers from './DirectProjectMembers.svelte';
  import OrganizationMembers from './OrganizationMembers.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { confirmAction } from '#lib/components/confirm.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { projectKeys } from '../projects/projectKeys';
  import { organizationKeys } from './organizationKeys';
  import AggregateBudgetPanel from '../budgets/AggregateBudgetPanel.svelte';
  import * as api from './api';

  const write = useAction();
  const intents = useMutationIntents();

  const access = useRole();
  const client = useQueryClient();
  let selected = $state('');
  let projectId = $state('');
  let name = $state('');
  let newName = $state('');
  let projectName = $state('');
  let renameProject = $state('');
  const organizations = createQuery(() => ({
    queryKey: organizationKeys.root,
    queryFn: ({ signal }) => api.listOrganizations(signal)
  }));
  const organization = $derived(
    organizations.data?.find((o) => o.id === selected)
  );
  const members = createQuery(() => ({
    queryKey: organizationKeys.members(selected),
    enabled: !!selected,
    queryFn: ({ signal }) => api.organizationMembers(selected, signal)
  }));
  const projects = createQuery(() => ({
    queryKey: organizationKeys.projects(selected),
    enabled: !!selected,
    queryFn: ({ signal }) => api.organizationProjects(selected, signal)
  }));
  const project = $derived(projects.data?.find((p) => p.id === projectId));
  const directMembers = createQuery(() => ({
    queryKey: organizationKeys.projectMembers(selected, projectId),
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
  async function removeMember(
    email: string,
    scope: string,
    action: () => Promise<unknown>
  ) {
    if (
      !(await confirmAction({
        title: `Remove ${email}?`,
        body: `They lose access granted through this ${scope}.`,
        confirmLabel: 'Remove member',
        danger: true
      }))
    )
      return;
    await run(action);
  }
  function run<T>(
    send: () => Promise<T>,
    after?: (value: T) => void | Promise<void>
  ) {
    return write.run(
      send,
      async (value) => {
        await after?.(value);
        notify('Saved.');
        await Promise.all([
          client.invalidateQueries(
            { queryKey: organizationKeys.root },
            { throwOnError: true }
          ),
          client.invalidateQueries(
            { queryKey: projectKeys.root },
            { throwOnError: true }
          )
        ]);
      },
      {
        failure: (cause, stage) =>
          stage === 'accept'
            ? `Saved, but refreshing failed. ${errorMessage(cause)}`
            : errorMessage(cause)
      }
    );
  }
</script>

<PageTitle title="Organizations" />
<PageHeader
  title="Organizations"
  description="Group projects under shared management and budgets. Installation roles still determine which operations each person can perform."
/>
{#if write.error}<Callout tone="danger">{write.error}</Callout>{/if}

{#if canCreate}
  <form
    onsubmit={(e) => {
      e.preventDefault();
      void run(
        () =>
          intents.submit('create-organization', newName.trim(), (input, id) =>
            api.createOrganization(input, id)
          ),
        (o) => {
          newName = '';
          client.setQueryData<api.Organization[]>(
            organizationKeys.root,
            (current = []) => [...current.filter((item) => item.id !== o.id), o]
          );
          selected = o.id;
        }
      );
    }}
  >
    <label class="form-field"
      >New organization name<input
        required
        maxlength="100"
        bind:value={newName}
        disabled={write.busy}
      /></label
    >
    <button class="button button-primary" disabled={write.busy}
      >Create organization</button
    >
  </form>
{/if}
<QueryView
  query={organizations}
  loading="Loading organizations…"
  failure={errorMessage(organizations.error)}
  emptyWhen={!organization}
>
  {#snippet empty()}
    <EmptyState title="No organizations yet"
      >Organizations group projects and people under one owner.</EmptyState
    >
  {/snippet}
  {#if organization}
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
          void run(
            () =>
              api.renameOrganization(selected, name.trim(), organization.etag),
            () => {
              name = '';
            }
          );
        }}
      >
        <label class="form-field"
          >Rename organization<input
            required
            maxlength="100"
            placeholder={organization.name}
            bind:value={name}
            disabled={write.busy}
          /></label
        ><button class="button button-secondary" disabled={write.busy}
          >Save name</button
        >
      </form>{/if}
    {#key selected}<AggregateBudgetPanel
        organizationId={selected}
        {editable}
      />{/key}
    <OrganizationMembers
      {members}
      {editable}
      busy={write.busy}
      onRemove={(member) =>
        removeMember(member.email, 'organization', () =>
          api.removeOrganizationMember(
            selected,
            member.user_id,
            organization.etag
          )
        )}
      onAdd={(memberId, memberRole) =>
        run(() =>
          api.putOrganizationMember(
            selected,
            memberId,
            memberRole,
            organization.etag
          )
        )}
    />
    <section class="section" aria-labelledby="organization-projects">
      <h2 id="organization-projects">Projects</h2>
      {#if projects.isError}<p class="inline-problem" role="alert">
          {errorMessage(projects.error)}
        </p>{/if}
      {#if editable}<form
          onsubmit={(e) => {
            e.preventDefault();
            const target = selected;
            void run(
              () =>
                intents.submit(
                  'create-project',
                  {
                    organization: target,
                    name: projectName.trim()
                  },
                  (input, requestId) =>
                    api.createOrganizationProject(
                      input.organization,
                      input.name,
                      requestId
                    )
                ),
              (p) => {
                projectName = '';
                client.setQueryData<
                  Awaited<ReturnType<typeof api.organizationProjects>>
                >(organizationKeys.projects(target), (current = []) => [
                  ...current.filter((item) => item.id !== p.id),
                  p
                ]);
                if (selected === target) projectId = p.id;
              }
            );
          }}
        >
          <label class="form-field"
            >New project name<input
              required
              maxlength="100"
              bind:value={projectName}
              disabled={write.busy}
            /></label
          ><button class="button button-primary" disabled={write.busy}
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
              void run(
                () =>
                  api.renameProject(
                    selected,
                    projectId,
                    renameProject.trim(),
                    project.etag
                  ),
                () => {
                  renameProject = '';
                }
              );
            }}
          >
            <label class="form-field"
              >Rename project<input
                required
                maxlength="100"
                placeholder={project.name}
                bind:value={renameProject}
                disabled={write.busy}
              /></label
            ><button class="button button-secondary" disabled={write.busy}
              >Save project name</button
            >
          </form>{/if}
        <DirectProjectMembers
          members={directMembers}
          {editable}
          busy={write.busy}
          onRemove={(member) =>
            removeMember(member.email, 'project', () =>
              api.removeProjectMember(
                selected,
                projectId,
                member.user_id,
                project.etag
              )
            )}
          onAdd={(memberId, memberRole) =>
            run(() =>
              api.putProjectMember(
                selected,
                projectId,
                memberId,
                memberRole,
                project.etag
              )
            )}
        />
      {:else}<p>No projects in this organization.</p>{/if}
    </section>
  {/if}
</QueryView>

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
    padding-top: 1.5rem;
  }
</style>
