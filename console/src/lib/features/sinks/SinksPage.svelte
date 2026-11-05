<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { listProjects } from '$lib/features/agents/api';
  import { agentKeys } from '$lib/features/agents/agentKeys';
  import { errorMessage } from '$lib/management/http';
  import ConfirmAction from '$lib/components/ConfirmAction.svelte';
  import SinkEditor from './SinkEditor.svelte';
  import { listSinks, retireSink, sinkKeys, type Sink } from './api';
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
  const current = $derived(
    sinks.data?.find((value) => value.id === selected) ?? null
  );
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
<section class="workflow">
  <h1 class="page-title">Signed fact exports</h1>
  <p>
    HTTPS JSON exports with exact-byte HMAC signing and an independent delivery
    queue
  </p>
  <button
    class="button button-secondary"
    type="button"
    onclick={() => {
      selected = '';
    }}>New export</button
  ><button class="button button-secondary" type="button" onclick={refresh}
    >Refresh delivery counts</button
  >
  {#if sinks.error}<p role="alert">{errorMessage(sinks.error)}</p>{/if}
  {#each sinks.data ?? [] as value (value.id)}<article class="card">
      <h2>{value.name}</h2>
      <p>
        {value.destination} · {value.retired_at
          ? 'Retired'
          : value.enabled
            ? 'Enabled'
            : 'Disabled'}
      </p>
      <p>
        {value.delivery.delivered} delivered, {value.delivery.failed} failed, {value
          .delivery.expired} expired. Last delivery {value.delivery
          .last_delivered_at ?? 'None'}
      </p>
      <button
        class="button button-secondary"
        type="button"
        onclick={() => {
          selected = value.id;
        }}>Edit export</button
      >
      {#if !value.retired_at && access.allows('DELETE /api/v1/sinks/{sink_id}')}<ConfirmAction
          label="Retire export"
          description={`Disable ${value.name}, cancel pending deliveries and remove its credential. Delivery history is retained`}
          identity={`${value.id}:${value.etag}`}
          run={(requestId) => retireSink(value, requestId)}
          done={refresh}
        />{/if}
    </article>{/each}
  {#if (!selected || current) && !current?.retired_at && access.allows(current ? 'PUT /api/v1/sinks/{sink_id}' : 'POST /api/v1/sinks')}{#key `${current?.id ?? 'new'}:${editorVersion}`}<SinkEditor
        {current}
        reload={() => {
          editorVersion += 1;
        }}
        projects={projects.data ?? []}
        {saved}
      />{/key}{/if}
</section>
