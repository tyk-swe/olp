<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { getTask } from './api';
  import { agentKeys } from './agentKeys';

  let { taskID }: { taskID: string } = $props();
  const task = createQuery(() => ({
    queryKey: agentKeys.task(taskID),
    queryFn: () => getTask(taskID),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<svelte:head><title>A2A task · OpenLLMProxy</title></svelte:head>
<a href={resolve('/a2a-tasks')}>All A2A tasks</a>
<header>
  <p class="eyebrow">Gateway</p>
  <h1>A2A task</h1>
  <p><code>{taskID}</code></p>
</header>
{#if task.error}
  <p role="alert">{errorMessage(task.error)}</p>
{:else if task.isFetching}
  <p>Loading task…</p>
{:else if task.data}
  <dl>
    <dt>State</dt>
    <dd>{task.data.state.replaceAll('_', ' ')}</dd>
    <dt>Callbacks admitted</dt>
    <dd>{task.data.callback_attempts ?? 'Unknown'}</dd>
    <dt>Project</dt>
    <dd><code>{task.data.project_id}</code></dd>
    <dt>API key</dt>
    <dd><code>{task.data.api_key_id}</code></dd>
    <dt>Agent publication</dt>
    <dd><code>{task.data.publication_id}</code></dd>
    <dt>Revision</dt>
    <dd><code>{task.data.revision_id}</code></dd>
    {#if task.data.agent_principal_id}
      <dt>Initiating agent</dt>
      <dd><code>{task.data.agent_principal_id}</code></dd>
    {/if}
    {#if task.data.member_principal_id}
      <dt>Consenting member</dt>
      <dd><code>{task.data.member_principal_id}</code></dd>
    {/if}
    {#if task.data.delegation_pair_id}
      <dt>Delegation pair</dt>
      <dd><code>{task.data.delegation_pair_id}</code></dd>
    {/if}
    <dt>Started</dt>
    <dd>{new Date(task.data.created_at).toLocaleString()}</dd>
    <dt>Deadline</dt>
    <dd>{new Date(task.data.deadline_at).toLocaleString()}</dd>
  </dl>
  <section aria-labelledby="remote-fees">
    <h2 id="remote-fees">Remote message fees</h2>
    <p>
      Observed amounts are accounted usage. Unresolved exposure remains reserved
      and is not a charge
    </p>
    {#if !task.data.remote_fees.length}
      <p>No remote message fees recorded</p>
    {:else}
      <div class="records">
        <table>
          <thead
            ><tr
              ><th>Run</th><th>State</th><th>Observed cost / charge</th><th
                >Unresolved cost / charge</th
              ><th>Started</th></tr
            ></thead
          >
          <tbody
            >{#each task.data.remote_fees as fee (fee.dispatch_id)}
              <tr
                ><td
                  ><a href={resolve(`/runs/${fee.run_id}`)}
                    ><code>{fee.run_id}</code></a
                  ></td
                >
                <td>{fee.state.replaceAll('_', ' ')}</td>
                <td aria-label="Observed cost and charge"
                  >{fee.observed_cost} / {fee.observed_charge}
                  {fee.currency}</td
                >
                <td aria-label="Unresolved cost and charge"
                  >{fee.remaining_cost} / {fee.remaining_charge}
                  {fee.currency}</td
                >
                <td>{new Date(fee.created_at).toLocaleString()}</td>
              </tr>
            {/each}</tbody
          >
        </table>
      </div>
    {/if}
  </section>
  <section aria-labelledby="callback-models">
    <h2 id="callback-models">Callback model usage</h2>
    {#if task.data.callback_attempts === null}<p>
        Accounting for earlier callbacks is unavailable
      </p>{/if}
    {#if !task.data.callback_models.length}<p>
        No model callback accounting recorded
      </p>{:else}
      <div class="records">
        <table>
          <thead
            ><tr
              ><th>Request</th><th>Pricing</th><th>Observed cost / charge</th
              ><th>Unresolved cost / charge</th></tr
            ></thead
          >
          <tbody
            >{#each task.data.callback_models as model, index (model.attempt_id ?? `${model.request_id}-${index}`)}
              <tr
                ><td><code>{model.request_id}</code></td><td
                  >{model.state.replaceAll('_', ' ')}</td
                >
                <td
                  >{model.observed_cost ?? 'Unknown'} / {model.observed_charge ??
                    'Unknown'}
                  {model.currency ?? ''}</td
                >
                <td
                  >{model.remaining_cost ?? 'Unknown'} / {model.remaining_charge ??
                    'Unknown'}
                  {model.currency ?? ''}</td
                ></tr
              >
            {/each}</tbody
          >
        </table>
      </div>
    {/if}
  </section>
  <section aria-labelledby="callback-tools">
    <h2 id="callback-tools">Callback tool usage</h2>
    {#if !task.data.callback_tools.length}<p>
        No tool callback accounting recorded
      </p>{:else}
      <div class="records">
        <table>
          <thead
            ><tr
              ><th>Run</th><th>State</th><th>Pricing</th><th
                >Observed cost / charge</th
              ><th>Unresolved cost / charge</th></tr
            ></thead
          >
          <tbody
            >{#each task.data.callback_tools as tool (tool.dispatch_id)}
              <tr
                ><td
                  ><a href={resolve(`/runs/${tool.run_id}`)}
                    ><code>{tool.run_id}</code></a
                  ></td
                ><td>{tool.state.replaceAll('_', ' ')}</td><td
                  >{tool.pricing_state}</td
                >
                <td
                  >{tool.observed_cost ?? 'Unknown'} / {tool.observed_charge ??
                    'Unknown'}
                  {tool.currency}</td
                >
                <td
                  >{tool.remaining_cost ?? 'Unknown'} / {tool.remaining_charge ??
                    'Unknown'}
                  {tool.currency}</td
                ></tr
              >
            {/each}</tbody
          >
        </table>
      </div>
    {/if}
  </section>
  <button type="button" onclick={() => task.refetch()}>Refresh</button>
{/if}

<style>
  header,
  section {
    margin: 1rem 0;
  }
  dl {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: 0.5rem 1rem;
  }
  dd {
    margin: 0;
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
</style>
