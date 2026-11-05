<script lang="ts">
  import { formatDate } from '$lib/format';
  import ScrollTable from '$lib/components/ScrollTable.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { page } from '$app/state';
  import { errorMessage } from '$lib/management/http';
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
<div class="page-header">
  <div>
    <p class="eyebrow">Agents</p>
    <h1 class="page-title">Delegations</h1>
    <p class="page-description">
      Delegating principals, agent principals and their shared budgets
    </p>
  </div>
</div>
<form
  class="filter-bar"
  onsubmit={(event) => {
    event.preventDefault();
    filters.set({ project, delegationID: delegationID.trim() });
  }}
>
  <label class="form-field"
    >Project <select name="project" class="form-field" bind:value={project}
      ><option value="">All authorized projects</option
      >{#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as value (value.id)}<option
          value={value.id}>{value.name}</option
        >{/each}</select
    ></label
  >
  <label class="form-field"
    >Delegation ID <input class="form-field" bind:value={delegationID} /></label
  >
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}<p class="inline-problem" role="alert">
    {errorMessage(projects.error)}
  </p>{/if}
{#if delegations.error}<p class="inline-problem" role="alert">
    {errorMessage(delegations.error)}
  </p>
{:else if delegations.isFetching}<div class="loading-state" role="status">
    Loading delegations…
  </div>
{:else if !delegations.data?.items.length}<div class="card empty-state">
    No delegations in this view
  </div>
{:else}
  <ScrollTable label="Delegations">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Delegation</th><th scope="col">Project</th><th
            scope="col">Delegating principal</th
          ><th scope="col">Agent principal</th><th scope="col">State</th><th
            scope="col">Pair spend / limit</th
          ><th scope="col">Expires</th></tr
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
            <td>{formatDate(delegation.expires_at)}</td>
          </tr>
        {/each}</tbody
      >
    </table>
  </ScrollTable>
{/if}
<nav class="page-actions list-pager" aria-label="Delegation pages">
  <button
    class="button button-secondary"
    type="button"
    disabled={!$filters.cursor || delegations.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
    >Newest</button
  >
  <button
    class="button button-secondary"
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
    class="button button-secondary"
    type="button"
    disabled={delegations.isFetching}
    onclick={() => delegations.refetch()}>Refresh</button
  >
</nav>
