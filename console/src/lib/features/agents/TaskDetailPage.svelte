<script lang="ts">
  import { formatDate, stateLabel } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { getTask, getRemoteTask, cancelRemoteTask } from './api';
  import { agentKeys } from './agentKeys';

  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  const access = useRole();
  const action = useAction();
  let remote = $state<Awaited<ReturnType<typeof getRemoteTask>> | null>(null);
  let observedAt = $state('');
  usePrincipalLifetime(() => {
    remote = null;
    observedAt = '';
  });
  const acceptRemote = (value: Awaited<ReturnType<typeof getRemoteTask>>) => {
    remote = value;
    observedAt = new Date().toLocaleString();
  };
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
<a class="text-button" href={resolve('/a2a-tasks')}>All A2A tasks</a>
<div class="page-header">
  <div>
    <p class="eyebrow">Agents</p>
    <h1 class="page-title">A2A task</h1>
    <p class="page-description"><code>{taskID}</code></p>
  </div>
</div>
{#if task.error}
  <p class="inline-problem" role="alert">{errorMessage(task.error)}</p>
{:else if task.isFetching}
  <p class="inline-status" role="status">Loading task…</p>
{:else if task.data}
  <dl class="detail-list">
    <dt>State</dt>
    <dd>{stateLabel(task.data.state)}</dd>
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
    <dd>{formatDate(task.data.created_at)}</dd>
    <dt>Deadline</dt>
    <dd>{formatDate(task.data.deadline_at)}</dd>
  </dl>
  <section class="section" aria-labelledby="remote-fees">
    <h2 id="remote-fees">Remote message fees</h2>
    <p>
      Observed amounts are accounted usage. Unresolved exposure remains reserved
      and is not a charge
    </p>
    {#if !task.data.remote_fees.length}
      <p class="inline-status">No remote message fees recorded</p>
    {:else}
      <ScrollTable label="Remote message fees">
        <table class="data-table">
          <thead
            ><tr
              ><th scope="col">Run</th><th scope="col">State</th><th scope="col"
                >Observed cost / charge</th
              ><th scope="col">Unresolved cost / charge</th><th scope="col"
                >Started</th
              ></tr
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
                <td>{stateLabel(fee.state)}</td>
                <td aria-label="Observed cost and charge"
                  >{fee.observed_cost} / {fee.observed_charge}
                  {fee.currency}</td
                >
                <td aria-label="Unresolved cost and charge"
                  >{fee.remaining_cost} / {fee.remaining_charge}
                  {fee.currency}</td
                >
                <td>{formatDate(fee.created_at)}</td>
              </tr>
            {/each}</tbody
          >
        </table>
      </ScrollTable>
    {/if}
  </section>
  <section class="section" aria-labelledby="callback-models">
    <h2 id="callback-models">Callback model usage</h2>
    {#if task.data.callback_attempts === null}<p>
        Accounting for earlier callbacks is unavailable
      </p>{/if}
    {#if !task.data.callback_models.length}<p class="inline-status">
        No model callback accounting recorded
      </p>{:else}
      <ScrollTable label="Callback model usage">
        <table class="data-table">
          <thead
            ><tr
              ><th scope="col">Request</th><th scope="col">Pricing</th><th
                scope="col">Observed cost / charge</th
              ><th scope="col">Unresolved cost / charge</th></tr
            ></thead
          >
          <tbody
            >{#each task.data.callback_models as model, index (model.attempt_id ?? `${model.request_id}-${index}`)}
              <tr
                ><td><code>{model.request_id}</code></td><td
                  >{stateLabel(model.state)}</td
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
      </ScrollTable>
    {/if}
  </section>
  <section class="section" aria-labelledby="callback-tools">
    <h2 id="callback-tools">Callback tool usage</h2>
    {#if !task.data.callback_tools.length}<p class="inline-status">
        No tool callback accounting recorded
      </p>{:else}
      <ScrollTable label="Callback tool usage">
        <table class="data-table">
          <thead
            ><tr
              ><th scope="col">Run</th><th scope="col">State</th><th scope="col"
                >Pricing</th
              ><th scope="col">Observed cost / charge</th><th scope="col"
                >Unresolved cost / charge</th
              ></tr
            ></thead
          >
          <tbody
            >{#each task.data.callback_tools as tool (tool.dispatch_id)}
              <tr
                ><td
                  ><a href={resolve(`/runs/${tool.run_id}`)}
                    ><code>{tool.run_id}</code></a
                  ></td
                ><td>{stateLabel(tool.state)}</td><td>{tool.pricing_state}</td>
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
      </ScrollTable>
    {/if}
  </section>
  <button
    class="button button-secondary"
    type="button"
    onclick={() => task.refetch()}>Refresh</button
  >
{/if}

{#if task.data && access.allows('POST /api/v1/a2a-tasks/{task_id}/cancel')}
  <section class="card">
    <h2>Remote task</h2>
    <button
      class="button button-secondary"
      type="button"
      disabled={action.busy}
      onclick={() => action.run(() => getRemoteTask(taskID), acceptRemote)}
      >Fetch remote status</button
    >
    <ConfirmAction
      danger
      label="Cancel remote task"
      description="Ask the remote agent to cancel this task"
      identity={taskID}
      {action}
      run={() => cancelRemoteTask(taskID)}
      done={async (result) => {
        acceptRemote(result);
        await task.refetch();
      }}
    />
    {#if remote}<p>
        Remote state {remote.status.state}, retrieved {observedAt}
      </p>
      <pre class="code-block">{JSON.stringify(remote, null, 2)}</pre>{/if}
  </section>
{/if}
