<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '$lib/api/http';
  import { listProjects } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
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
<header>
  <p class="eyebrow">Gateway</p>
  <h1>Tool calls</h1>
  <p>
    Attempts and settlement history for your projects. Unresolved amounts remain
    reserved until the outcome is known. Observed amounts are already accounted
    usage and must not be billed again.
  </p>
</header>
<form
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
  <label>
    Project
    <select name="project">
      <option value="">All authorized projects</option>
      {#each projects.data ?? [] as value (value.id)}
        <option value={value.id}>{value.name}</option>
      {/each}
    </select>
  </label>
  <label>
    API key UUID (optional)
    <input name="key" pattern="[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}" />
  </label>
  <button type="submit">Filter</button>
</form>
{#if projects.error}
  <p role="alert">{errorMessage(projects.error)}</p>
{/if}
{#if calls.error}
  <p role="alert">{errorMessage(calls.error)}</p>
{:else if calls.isPending}
  <p>Loading tool calls…</p>
{:else if !calls.data?.items.length}
  <p>No tool calls in this view.</p>
{:else}
  <div class="records">
    <table>
      <thead>
        <tr>
          <th>Tool / attempt</th>
          <th>Outcome / debit</th>
          <th>Unit price</th>
          <th>Observed cost / charge</th>
          <th>Unresolved cost / charge</th>
          <th>Started</th>
        </tr>
      </thead>
      <tbody>
        {#each calls.data.items as value (value.id)}
          <tr>
            <td>
              <code>{value.tool_alias}</code>
              <details>
                <summary>Attempt {value.id}</summary>
                <dl>
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
                  <dd>{value.completed_at ?? 'Pending'}</dd>
                </dl>
                {#if value.identity_decisions.length}
                  <h2>Identity decisions</h2>
                  {#each value.identity_decisions as decision (decision.hook)}
                    <p>{decision.hook}: {decision.outcome}</p>
                  {/each}
                {/if}
                {#if value.guardrail_decisions.length}
                  <h2>Guardrail decisions</h2>
                  <pre>{JSON.stringify(
                      value.guardrail_decisions,
                      null,
                      2
                    )}</pre>
                {/if}
              </details>
            </td>
            <td
              >{value.status.replaceAll('_', ' ')}<br
              />{value.debit_state.replaceAll('_', ' ')}</td
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
            <td>{new Date(value.started_at).toLocaleString()}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
<nav aria-label="Tool call pages">
  <button
    type="button"
    disabled={!$filters.cursor || calls.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
    >Newest</button
  >
  <button
    type="button"
    disabled={!calls.data?.nextCursor || calls.isFetching}
    onclick={() =>
      filters.update((current) => ({
        ...current,
        cursor: calls.data?.nextCursor ?? undefined
      }))}>Older</button
  >
  <button
    type="button"
    disabled={calls.isFetching}
    onclick={() => calls.refetch()}>Refresh</button
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
  details {
    margin-top: 0.5rem;
    min-width: 12rem;
  }
  dd {
    margin: 0 0 0.5rem;
  }
  pre {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  nav {
    display: flex;
    gap: 0.5rem;
    margin-top: 1rem;
  }
</style>
