<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { errorMessage } from '$lib/management/http';
  import ConfirmAction from '$lib/components/ConfirmAction.svelte';
  import MCPServerEditor from './MCPServerEditor.svelte';
  import {
    listServers,
    getServer,
    listProjects,
    retireServer,
    type MCPServer
  } from './api';
  import { agentKeys } from './agentKeys';
  const access = useRole();
  const client = useQueryClient();
  const servers = createQuery(() => ({
    queryKey: agentKeys.servers(),
    queryFn: listServers
  }));
  const projects = createQuery(() => ({
    queryKey: agentKeys.projects(),
    queryFn: listProjects
  }));
  let selected = $state('');
  let editorVersion = $state(0);
  let message = $state('');
  const detail = createQuery(() => ({
    queryKey: agentKeys.server(selected),
    queryFn: () => getServer(selected),
    enabled: Boolean(selected)
  }));
  const current = $derived(detail.data ?? null);
  async function saved(server: MCPServer) {
    client.setQueryData(agentKeys.server(server.id), server);
    selected = server.id;
    message = 'Server certified and available to toolsets';
    await client.invalidateQueries({ queryKey: agentKeys.servers() });
  }
</script>

<svelte:head><title>MCP servers · OpenLLMProxy</title></svelte:head>
<section class="workflow">
  <h1 class="page-title">MCP servers</h1>
  <a href={resolve('/agents')}>Agent publications and toolsets</a>
  {#if servers.error || projects.error}<p role="alert">
      {errorMessage(servers.error ?? projects.error)}
    </p>{/if}
  {#if message}<p role="status">{message}</p>{/if}
  <button
    class="button button-secondary"
    type="button"
    onclick={() => {
      selected = '';
    }}>New server</button
  >
  {#each servers.data ?? [] as server (server.id)}
    <article class="card">
      <h2>{server.name}</h2>
      <p>
        {server.transport} · {server.retired_at
          ? 'Retired'
          : server.enabled
            ? 'Enabled'
            : 'Disabled'}
      </p>
      <p>Certified revision {server.revision}</p>
      <button
        class="button button-secondary"
        type="button"
        onclick={() => {
          selected = server.id;
        }}>Inspect and edit</button
      >
      {#if !server.retired_at && access.allows('DELETE /api/v1/mcp-servers/{server_id}')}<ConfirmAction
          label="Retire server"
          description={`Retire ${server.name} and stop new use of this server`}
          identity={`${server.id}:${server.etag}`}
          run={(requestId) => retireServer(server, requestId)}
          done={async () => {
            await client.invalidateQueries({ queryKey: agentKeys.servers() });
          }}
        />{/if}
    </article>
  {/each}
  {#if current}<details>
      <summary>Certified catalog</summary>
      <pre>{JSON.stringify(current.catalog, null, 2)}</pre>
    </details>{/if}
  {#if (!selected || current) && !current?.retired_at && access.allows(current ? 'PUT /api/v1/mcp-servers/{server_id}' : 'POST /api/v1/mcp-servers')}
    {#key `${current?.id ?? 'new'}:${editorVersion}`}<MCPServerEditor
        {current}
        reload={() => {
          editorVersion += 1;
        }}
        projects={projects.data ?? []}
        onsaved={saved}
      />{/key}
  {/if}
</section>
