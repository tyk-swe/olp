<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
  import { formatDate, stateLabel } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { agentKeys } from './agentKeys';
  import { listToolCalls } from './api';
  import ListPager from './ListPager.svelte';

  let project = $state('');
  let key = $state('');
  let applied = $state.raw({ project: '', key: '' });
  const pagination = $state(emptyCursorHistory());
  const projects = createQuery(projectQueries.all);
  const calls = createQuery(() => ({
    queryKey: agentKeys.toolCalls(
      applied.project,
      applied.key,
      pagination.cursor
    ),
    queryFn: () =>
      listToolCalls(applied.project, applied.key, pagination.cursor)
  }));
</script>

<PageTitle title="Tool calls" />
<form
  class="filter-bar"
  onsubmit={(event) => {
    event.preventDefault();
    applied = { project, key: key.trim() };
    resetCursor(pagination);
  }}
>
  <label class="form-field">
    Project
    <select bind:value={project}>
      <option value="">All authorized projects</option>
      {#each projects.data ?? [] as value (value.id)}
        <option value={value.id}>{value.name}</option>
      {/each}
    </select>
  </label>
  <label class="form-field">
    API key UUID (optional)
    <input
      bind:value={key}
      pattern={'[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}'}
    />
  </label>
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}
  <Callout tone="danger">{errorMessage(projects.error)}</Callout>
{/if}
<QueryView
  query={calls}
  loading="Loading tool calls…"
  failure={errorMessage(calls.error)}
  emptyWhen={!calls.data?.items.length}
>
  {#snippet empty()}
    <EmptyState title="No tool calls in this view" />
  {/snippet}
  <ScrollTable label="Tool calls">
    <table class="data-table">
      <thead>
        <tr>
          <th scope="col">Tool / attempt</th>
          <th scope="col">Outcome / debit</th>
          <th scope="col">Unit price</th>
          <th scope="col">Observed cost / charge</th>
          <th scope="col">Unresolved cost / charge</th>
          <th scope="col">Started</th>
        </tr>
      </thead>
      <tbody>
        {#each calls.data?.items ?? [] as value (value.id)}
          <tr>
            <td>
              <code>{value.tool_alias}</code>
              <details>
                <summary>Attempt {value.id}</summary>
                <a
                  class="table-link"
                  href={resolve(`activity/tool-calls/${value.id}`)}
                >
                  Inspect and resolve
                </a>

                <dl class="detail-list">
                  <dt>Dispatch</dt>
                  <dd><code>{value.dispatch_id}</code></dd>
                  <dt>Project</dt>
                  <dd><code>{value.project_id}</code></dd>
                  <dt>API key</dt>
                  <dd><code>{value.api_key_id}</code></dd>
                  <dt>Toolset revision</dt>
                  <dd><code>{value.toolset_revision_id}</code></dd>
                  <dt>Server revision</dt>
                  <dd><code>{value.server_revision_id}</code></dd>
                  <dt>Input digest</dt>
                  <dd><code>{value.input_digest}</code></dd>
                  <dt>Result digest</dt>
                  <dd><code>{value.result_digest ?? 'Not observed'}</code></dd>
                  <dt>Completed</dt>
                  <dd>
                    {value.completed_at
                      ? formatDate(value.completed_at)
                      : 'Pending'}
                  </dd>
                </dl>
                {#if value.identity_decisions.length}
                  <h2>Identity decisions</h2>
                  {#each value.identity_decisions as decision (decision.hook)}
                    <p>{decision.hook}: {decision.outcome}</p>
                  {/each}
                {/if}
                {#if value.guardrail_decisions.length}
                  <h2>Guardrail decisions</h2>
                  <pre class="code-block">{JSON.stringify(
                      value.guardrail_decisions,
                      null,
                      2
                    )}</pre>
                {/if}
              </details>
            </td>
            <td>
              {stateLabel(value.status)}<br />{stateLabel(value.debit_state)}
            </td>
            <td>
              {value.unit_price ?? 'Unpriced'}
              {value.currency ?? ''}<br />{value.pricing_state}
            </td>
            <td aria-label="Observed cost and charge">
              {value.observed_cost} / {value.observed_charge}
            </td>
            <td aria-label="Unresolved cost and charge">
              {value.remaining_cost} / {value.remaining_charge}
            </td>
            <td>{formatDate(value.started_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>
<ListPager {pagination} query={calls} label="Tool call pages" />
