<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { formatDate } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { page } from '$app/state';
  import { errorMessage } from '#lib/management/http.js';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { listDelegations } from './api';
  import { agentKeys } from './agentKeys';
  import ListPager from './ListPager.svelte';
  import { loadingWhileFetching } from './listQuery';

  const initialProject = page.url?.searchParams.get('project_id') ?? '';
  const initialDelegationID = page.url?.searchParams.get('delegation_id') ?? '';
  let project = $state(initialProject);
  let delegationID = $state(initialDelegationID);
  let applied = $state.raw({
    project: initialProject,
    delegationID: initialDelegationID
  });
  const pagination = $state(emptyCursorHistory());
  const projects = createQuery(projectQueries.memberships);
  const delegations = createQuery(() => ({
    queryKey: agentKeys.delegations(
      applied.project,
      pagination.cursor,
      applied.delegationID
    ),
    queryFn: () =>
      listDelegations(applied.project, pagination.cursor, applied.delegationID),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<PageTitle title="Delegations" />
<form
  class="filter-bar"
  onsubmit={(event) => {
    event.preventDefault();
    applied = { project, delegationID: delegationID.trim() };
    resetCursor(pagination);
  }}
>
  <label class="form-field">
    Project <select name="project" class="form-field" bind:value={project}>
      <option value="">
        All authorized projects
      </option>{#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as value (value.id)}<option
          value={value.id}
        >
          {value.name}
        </option>{/each}
    </select>
  </label>
  <label class="form-field">
    Delegation ID <input class="form-field" bind:value={delegationID} />
  </label>
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}<Callout tone="danger">
    {errorMessage(projects.error)}
  </Callout>{/if}
<QueryView
  query={loadingWhileFetching(delegations)}
  loading="Loading delegations…"
  emptyWhen={!delegations.data?.items.length}
>
  {#snippet empty()}<EmptyState title="No delegations in this view" />{/snippet}
  <ScrollTable label="Delegations">
    <table class="data-table">
      <thead>
        <tr>
          <th scope="col">Delegation</th><th scope="col">Project</th><th
            scope="col"
          >
            Delegating principal
          </th><th scope="col">Agent principal</th><th scope="col">State</th><th
            scope="col"
          >
            Pair spend / limit
          </th><th scope="col">Expires</th>
        </tr>
      </thead>
      <tbody>
        {#each delegations.data?.items ?? [] as delegation (delegation.id)}
          <tr>
            <td><code>{delegation.id}</code></td>
            <td>
              {projects.data?.find(
                (project) => project.id === delegation.project_id
              )?.name ?? delegation.project_id}
            </td>
            <td>
              {#if delegation.member_id}Member <code>
                  {delegation.member_id}
                </code><br />{/if}API key <code>{delegation.api_key_id}</code>
            </td>
            <td><code>{delegation.agent_id}</code></td>
            <td>
              {#if delegation.revoked_at}Revoked{:else if delegation.member_authorized === false}Member
                authority inactive{:else if delegation.api_key_revoked_at}API
                key revoked{:else if delegation.agent_revoked_at}Agent revoked{:else if delegation.consent_pending}Consent
                pending{:else if new Date(delegation.expires_at).getTime() <= Date.now()}Expired{:else if delegation.consumed_at}Consumed{:else}Issued{/if}
            </td>
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
              {#if delegation.budget.unpriced_attempts || delegation.budget.uncharged_attempts}<div>
                  Some usage has no price or charge
                </div>{/if}
            </td>
            <td>{formatDate(delegation.expires_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>
<ListPager {pagination} query={delegations} label="Delegation pages" />
