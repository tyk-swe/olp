<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { page } from '$app/state';
  import { errorMessage } from '$lib/api/http';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { listDelegations } from './api';
  import { agentKeys } from './agentKeys';

  const initialProject = page.url?.searchParams.get('project_id') ?? '';
  const initialDelegationID = page.url?.searchParams.get('delegation_id') ?? '';
  let project = $state(initialProject);
  let delegationID = $state(initialDelegationID);
  const filters = writable<{
    project: string;
    delegationID: string;
    cursor?: string;
  }>({
    project: initialProject,
    delegationID: initialDelegationID
  });
  const projects = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
  const delegations = createQuery(() => ({
    queryKey: agentKeys.delegations(
      $filters.project,
      $filters.cursor,
      $filters.delegationID
    ),
    queryFn: () =>
      listDelegations($filters.project, $filters.cursor, $filters.delegationID),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<svelte:head><title>Delegations · OpenLLMProxy</title></svelte:head>
<header>
  <p class="eyebrow">Gateway</p>
  <h1>Delegations</h1>
  <p>Delegating principals, agent principals and their shared budgets</p>
</header>
<form
  onsubmit={(event) => {
    event.preventDefault();
    filters.set({ project, delegationID: delegationID.trim() });
  }}
>
  <label
    >Project <select name="project" class="form-field" bind:value={project}
      ><option value="">All authorized projects</option
      >{#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as value (value.id)}<option
          value={value.id}>{value.name}</option
        >{/each}</select
    ></label
  >
  <label
    >Delegation ID <input class="form-field" bind:value={delegationID} /></label
  >
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}<p role="alert">{errorMessage(projects.error)}</p>{/if}
{#if delegations.error}<p role="alert">{errorMessage(delegations.error)}</p>
{:else if delegations.isFetching}<p>Loading delegations…</p>
{:else if !delegations.data?.items.length}<p>No delegations in this view</p>
{:else}
  <div class="records">
    <table>
      <thead
        ><tr
          ><th>Delegation</th><th>Project</th><th>Delegating principal</th><th
            >Agent principal</th
          ><th>State</th><th>Pair spend / limit</th><th>Expires</th></tr
        ></thead
      >
      <tbody
        >{#each delegations.data.items as delegation (delegation.id)}
          <tr>
            <td><code>{delegation.id}</code></td>
            <td
              >{projects.data?.find(
                (project) => project.id === delegation.project_id
              )?.name ?? delegation.project_id}</td
            >
            <td
              >{#if delegation.member_id}Member <code
                  >{delegation.member_id}</code
                ><br />{/if}API key <code>{delegation.api_key_id}</code></td
            >
            <td><code>{delegation.agent_id}</code></td>
            <td
              >{#if delegation.revoked_at}Revoked{:else if delegation.member_authorized === false}Member
                authority inactive{:else if delegation.api_key_revoked_at}API
                key revoked{:else if delegation.agent_revoked_at}Agent revoked{:else if delegation.consent_pending}Consent
                pending{:else if new Date(delegation.expires_at).getTime() <= Date.now()}Expired{:else if delegation.consumed_at}Consumed{:else}Issued{/if}</td
            >
            <td>
              <div>
                Daily: {delegation.budget.daily.accrued} / {delegation
                  .budget_policy?.daily_cost_limit ?? 'Unlimited'}
              </div>
              <div>
                Weekly: {delegation.budget.weekly?.accrued ?? 'Unavailable'} / {delegation
                  .budget_policy?.weekly_cost_limit ?? 'Unlimited'}
              </div>
              <div>
                Monthly: {delegation.budget.monthly.accrued} / {delegation
                  .budget_policy?.monthly_cost_limit ?? 'Unlimited'}
              </div>
              <div>Basis: {delegation.budget.cost_basis ?? 'cost'}</div>
              {#if delegation.budget.unpriced_attempts || delegation.budget.uncharged_attempts}<div
                >
                  Some usage has no price or charge
                </div>{/if}
            </td>
            <td>{new Date(delegation.expires_at).toLocaleString()}</td>
          </tr>
        {/each}</tbody
      >
    </table>
  </div>
{/if}
<nav aria-label="Delegation pages">
  <button
    type="button"
    disabled={!$filters.cursor || delegations.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
    >Newest</button
  >
  <button
    type="button"
    disabled={!!delegations.error ||
      !delegations.data?.nextCursor ||
      delegations.isFetching}
    onclick={() =>
      filters.update((current) => ({
        ...current,
        cursor: delegations.data?.nextCursor ?? undefined
      }))}>Older</button
  >
  <button
    type="button"
    disabled={delegations.isFetching}
    onclick={() => delegations.refetch()}>Refresh</button
  >
</nav>

<style>
  header,
  form {
    margin-bottom: 1rem;
  }
  form {
    display: flex;
    align-items: end;
    flex-wrap: wrap;
    gap: 1rem;
  }
  label {
    display: grid;
    gap: 0.25rem;
  }
  .records {
    overflow-x: auto;
  }
  table {
    width: 100%;
    text-align: left;
  }
  td,
  th {
    padding: 0.75rem;
    vertical-align: top;
  }
  code {
    overflow-wrap: anywhere;
  }
  nav {
    display: flex;
    gap: 0.5rem;
    margin-top: 1rem;
  }
</style>
