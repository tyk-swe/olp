<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import ConfirmAction from '$lib/components/ConfirmAction.svelte';
  import ScrollTable from '$lib/components/ScrollTable.svelte';
  import { confirmDiscardChanges } from '$lib/editing/unsavedChanges';
  import { agentKeys } from '$lib/features/agents/agentKeys';
  import { listProjects } from '$lib/features/agents/api';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { formatDate, formatInteger } from '$lib/format';
  import { errorMessage } from '$lib/management/http';
  import SinkEditor from './SinkEditor.svelte';
  import { listSinks, retireSink, type Sink } from './api';
  import { sinkKeys } from './sinkKeys';

  const access = useRole();
  const client = useQueryClient();
  const sinks = createQuery(() => ({
    queryKey: sinkKeys.all,
    queryFn: listSinks
  }));
  const projects = createQuery(() => ({
    queryKey: agentKeys.projects(),
    queryFn: listProjects
  }));
  let selected = $state('');
  let editorVersion = $state(0);
  let editor = $state<ReturnType<typeof SinkEditor>>();
  const current = $derived(
    sinks.data?.find((value) => value.id === selected) ?? null
  );

  function select(id: string) {
    if (!confirmDiscardChanges(editor?.isDirty() ?? false)) return;
    selected = id;
    editorVersion += 1;
  }
  async function saved(value: Sink) {
    client.setQueryData<Sink[]>(sinkKeys.all, (items = []) =>
      items.some((item) => item.id === value.id)
        ? items.map((item) => (item.id === value.id ? value : item))
        : [...items, value]
    );
    selected = value.id;
    await refresh();
  }
  async function refresh() {
    await sinks.refetch();
  }
</script>

<svelte:head><title>Signed fact exports · OpenLLMProxy</title></svelte:head>
<div class="page-header">
  <div>
    <p class="eyebrow">Operations</p>
    <h1 class="page-title">Signed fact exports</h1>
    <p class="page-description">
      HTTPS JSON exports with exact-byte HMAC signing and an independent
      delivery queue.
    </p>
  </div>
  <div class="page-actions">
    <button class="button button-secondary" type="button" onclick={refresh}
      >Refresh delivery counts</button
    >
    {#if access.allows('POST /api/v1/sinks')}
      <button
        class="button button-primary"
        type="button"
        onclick={() => select('')}>New export</button
      >
    {/if}
  </div>
</div>

{#if sinks.error}
  <p class="inline-problem" role="alert">{errorMessage(sinks.error)}</p>
{:else if sinks.isPending}
  <div class="loading-state" role="status">Loading exports…</div>
{:else if !sinks.data?.length}
  <div class="card empty-state">No signed fact exports yet.</div>
{:else}
  <ScrollTable label="Signed fact exports">
    <table class="data-table">
      <caption class="sr-only">Signed fact exports and delivery counts</caption>
      <thead>
        <tr>
          <th scope="col">Export</th>
          <th scope="col">State</th>
          <th scope="col">Delivered</th>
          <th scope="col">Failed</th>
          <th scope="col">Expired</th>
          <th scope="col">Last delivery</th>
          <th scope="col">Actions</th>
        </tr>
      </thead>
      <tbody>
        {#each sinks.data as value (value.id)}
          <tr>
            <td>
              <strong>{value.name}</strong><br /><code>{value.destination}</code
              >
            </td>
            <td>
              {#if value.retired_at}
                <span class="badge">Retired</span>
              {:else if value.enabled}
                <span class="badge success">Enabled</span>
              {:else}
                <span class="badge">Disabled</span>
              {/if}
            </td>
            <td>{formatInteger(value.delivery.delivered)}</td>
            <td>{formatInteger(value.delivery.failed)}</td>
            <td>{formatInteger(value.delivery.expired)}</td>
            <td
              >{value.delivery.last_delivered_at
                ? formatDate(value.delivery.last_delivered_at)
                : 'None'}</td
            >
            <td>
              <div class="page-actions">
                {#if !value.retired_at && access.allows('PUT /api/v1/sinks/{sink_id}')}
                  <button
                    class="button button-secondary"
                    type="button"
                    onclick={() => select(value.id)}>Edit export</button
                  >
                {/if}
                {#if !value.retired_at && access.allows('DELETE /api/v1/sinks/{sink_id}')}
                  <ConfirmAction
                    danger
                    label="Retire export"
                    description={`Disable ${value.name}, cancel pending deliveries and remove its credential. Delivery history is retained.`}
                    identity={`${value.id}:${value.etag}`}
                    run={(requestId) => retireSink(value, requestId)}
                    done={refresh}
                  />
                {/if}
              </div>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}

{#if (!selected || current) && !current?.retired_at && access.allows(current ? 'PUT /api/v1/sinks/{sink_id}' : 'POST /api/v1/sinks')}
  {#key `${current?.id ?? 'new'}:${editorVersion}`}
    <SinkEditor
      bind:this={editor}
      {current}
      reload={() => {
        editorVersion += 1;
      }}
      projects={projects.data ?? []}
      {saved}
    />
  {/key}
{/if}
