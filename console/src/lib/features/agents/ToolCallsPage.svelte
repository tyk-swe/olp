<script lang="ts">
  import { formatDate, stateLabel } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '#lib/management/http.js';
  import { listProjects } from '#lib/features/access/projects/api.js';
  import { projectKeys } from '#lib/features/access/projects/projectKeys.js';
  import { agentKeys } from './agentKeys';
  import { listToolCalls } from './api';

  const filters = writable<{
    project: string;
    key: string;
    cursor?: string;
  }>({ project: '', key: '' });
  const projects = createQuery(() => ({
    queryKey: projectKeys.all,
    queryFn: ({ signal }) => listProjects(signal)
  }));
  const calls = createQuery(() => ({
    queryKey: agentKeys.toolCalls(
      $filters.project,
      $filters.key,
      $filters.cursor
    ),
    queryFn: () =>
      listToolCalls($filters.project, $filters.key, $filters.cursor)
  }));
</script>

<svelte:head><title>Tool calls · OpenLLMProxy</title></svelte:head>
<form
  class="filter-bar"
  onsubmit={(event) => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const project = form.get('project');
    const key = form.get('key');
    filters.set({
      project: typeof project === 'string' ? project : '',
      key: typeof key === 'string' ? key.trim() : ''
    });
  }}
>
  <label class="form-field">
    Project
    <select name="project">
      <option value="">All authorized projects</option>
      {#each projects.data ?? [] as value (value.id)}
        <option value={value.id}>{value.name}</option>
      {/each}
    </select>
  </label>
  <label class="form-field">
    API key UUID (optional)
    <input name="key" pattern="[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}" />
  </label>
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}
  <p class="inline-problem" role="alert">{errorMessage(projects.error)}</p>
{/if}
{#if calls.error}
  <p class="inline-problem" role="alert">{errorMessage(calls.error)}</p>
{:else if calls.isPending}
  <div class="loading-state" role="status">Loading tool calls…</div>
{:else if !calls.data?.items.length}
  <div class="card empty-state">No tool calls in this view.</div>
{:else}
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
        {#each calls.data.items as value (value.id)}
          <tr>
            <td>
              <code>{value.tool_alias}</code>
              <details>
                <summary>Attempt {value.id}</summary>
                <a href={resolve(`activity/tool-calls/${value.id}`)}
                  >Inspect and resolve</a
                >

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
            <td
              >{stateLabel(value.status)}<br />{stateLabel(
                value.debit_state
              )}</td
            >
            <td
              >{value.unit_price ?? 'Unpriced'}
              {value.currency ?? ''}<br />{value.pricing_state}</td
            >
            <td aria-label="Observed cost and charge"
              >{value.observed_cost} / {value.observed_charge}</td
            >
            <td aria-label="Unresolved cost and charge"
              >{value.remaining_cost} / {value.remaining_charge}</td
            >
            <td>{formatDate(value.started_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}
<nav class="page-actions list-pager" aria-label="Tool call pages">
  <button
    class="button button-secondary"
    type="button"
    disabled={!$filters.cursor || calls.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
    >Newest</button
  >
  <button
    class="button button-secondary"
    type="button"
    disabled={!calls.data?.nextCursor || calls.isFetching}
    onclick={() =>
      filters.update((current) => ({
        ...current,
        cursor: calls.data?.nextCursor ?? undefined
      }))}>Older</button
  >
  <button
    class="button button-secondary"
    type="button"
    disabled={calls.isFetching}
    onclick={() => calls.refetch()}>Refresh</button
  >
</nav>
