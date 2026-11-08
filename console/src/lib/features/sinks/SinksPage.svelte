<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { confirmDiscardChanges } from '#lib/editing/unsavedChanges.js';
  import { agentKeys } from '#lib/features/agents/agentKeys.js';
  import { listProjects } from '#lib/features/agents/api.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { formatDate, formatInteger } from '#lib/format.js';
  import { errorMessage } from '#lib/management/http.js';
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
  let editorOpen = $state(false);
  let editor = $state<ReturnType<typeof SinkEditor>>();
  const current = $derived(
    sinks.data?.find((value) => value.id === selected) ?? null
  );

  function select(id: string) {
    if (!confirmDiscardChanges(editor?.isDirty() ?? false)) return;
    selected = id;
    editorVersion += 1;
    editorOpen = true;
  }
  async function saved(value: Sink) {
    client.setQueryData<Sink[]>(sinkKeys.all, (items = []) =>
      items.some((item) => item.id === value.id)
        ? items.map((item) => (item.id === value.id ? value : item))
        : [...items, value]
    );
    editorOpen = false;
    selected = '';
    notify(`Saved ${value.name}`);
    await refresh();
  }
  async function refresh() {
    await sinks.refetch();
  }
</script>

<svelte:head><title>Signed fact exports · OpenLLMProxy</title></svelte:head>
<div class="toolbar">
  <p class="section-help">
    Sends signed usage and audit records to an HTTPS endpoint so another system
    can verify them.
  </p>
  <div class="page-actions">
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
</div>
{#if sinks.error}
  <Callout tone="danger">{errorMessage(sinks.error)}</Callout>
{:else if sinks.isPending}
  <div class="loading-state" role="status">Loading exports…</div>
{:else if !sinks.data?.length}
  <EmptyState title="No signed exports yet"
    >Create one to deliver signed records to your own endpoint.</EmptyState
  >
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
                    onclick={() => select(value.id)}>Edit</button
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

{#if editorOpen && (!selected || current) && !current?.retired_at && access.allows(current ? 'PUT /api/v1/sinks/{sink_id}' : 'POST /api/v1/sinks')}
  <Dialog
    title={current ? `Edit ${current.name}` : 'New signed export'}
    placement="side"
    size="wide"
    canClose={() => confirmDiscardChanges(editor?.isDirty() ?? false)}
    onClose={() => {
      editorOpen = false;
      selected = '';
    }}
  >
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
  </Dialog>
{/if}
