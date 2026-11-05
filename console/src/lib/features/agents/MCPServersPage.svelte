<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import ConfirmAction from '$lib/components/ConfirmAction.svelte';
  import ScrollTable from '$lib/components/ScrollTable.svelte';
  import { confirmDiscardChanges } from '$lib/editing/unsavedChanges';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { stateLabel } from '$lib/format';
  import { errorMessage } from '$lib/management/http';
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
  let editor = $state<ReturnType<typeof MCPServerEditor>>();
  const detail = createQuery(() => ({
    queryKey: agentKeys.server(selected),
    queryFn: () => getServer(selected),
    enabled: Boolean(selected)
  }));
  const current = $derived(detail.data ?? null);
  const canEdit = $derived(
    access.allows('PUT /api/v1/mcp-servers/{server_id}')
  );

  function select(id: string) {
    if (!confirmDiscardChanges(editor?.isDirty() ?? false)) return;
    selected = id;
    editorVersion += 1;
  }
  async function saved(server: MCPServer) {
    client.setQueryData(agentKeys.server(server.id), server);
    selected = server.id;
    message = 'Server certified and available to toolsets';
    await client.invalidateQueries({ queryKey: agentKeys.servers() });
  }
</script>

<svelte:head><title>MCP servers · OpenLLMProxy</title></svelte:head>
<div class="page-header">
  <div>
    <p class="eyebrow">Agents</p>
    <h1 class="page-title">MCP servers</h1>
    <p class="page-description">
      Register and certify servers, then select their tools in
      <a class="text-button" href={resolve('/agents')}
        >Agent publications and toolsets</a
      >.
    </p>
  </div>
  {#if access.allows('POST /api/v1/mcp-servers')}
    <button
      class="button button-primary"
      type="button"
      onclick={() => select('')}>New server</button
    >
  {/if}
</div>

{#if projects.error}
  <p class="inline-problem" role="alert">{errorMessage(projects.error)}</p>
{/if}
{#if message}<p class="success-message" role="status">{message}</p>{/if}

{#if servers.error}
  <p class="inline-problem" role="alert">{errorMessage(servers.error)}</p>
{:else if servers.isPending}
  <div class="loading-state" role="status">Loading servers…</div>
{:else if !servers.data?.length}
  <div class="card empty-state">No MCP servers registered yet.</div>
{:else}
  <ScrollTable label="MCP servers">
    <table class="data-table">
      <caption class="sr-only">Registered MCP servers</caption>
      <thead>
        <tr>
          <th scope="col">Server</th>
          <th scope="col">Transport</th>
          <th scope="col">State</th>
          <th scope="col">Certified revision</th>
          <th scope="col">Actions</th>
        </tr>
      </thead>
      <tbody>
        {#each servers.data as server (server.id)}
          <tr>
            <td><strong>{server.name}</strong></td>
            <td>{stateLabel(server.transport)}</td>
            <td>
              {#if server.retired_at}
                <span class="badge">Retired</span>
              {:else if server.enabled}
                <span class="badge success">Enabled</span>
              {:else}
                <span class="badge">Disabled</span>
              {/if}
            </td>
            <td>{server.revision}</td>
            <td>
              <div class="page-actions">
                <button
                  class="button button-secondary"
                  type="button"
                  onclick={() => select(server.id)}
                  >{canEdit ? 'Inspect and edit' : 'Inspect'}</button
                >
                {#if !server.retired_at && access.allows('DELETE /api/v1/mcp-servers/{server_id}')}
                  <ConfirmAction
                    danger
                    label="Retire server"
                    description={`Retire ${server.name} and stop new use of this server.`}
                    identity={`${server.id}:${server.etag}`}
                    run={(requestId) => retireServer(server, requestId)}
                    done={async () => {
                      await client.invalidateQueries({
                        queryKey: agentKeys.servers()
                      });
                    }}
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

{#if current}
  <details class="card stack-card">
    <summary>Certified catalog</summary>
    <pre class="code-block">{JSON.stringify(current.catalog, null, 2)}</pre>
  </details>
{/if}
{#if (!selected || current) && !current?.retired_at && access.allows(current ? 'PUT /api/v1/mcp-servers/{server_id}' : 'POST /api/v1/mcp-servers')}
  {#key `${current?.id ?? 'new'}:${editorVersion}`}
    <MCPServerEditor
      bind:this={editor}
      {current}
      reload={() => {
        editorVersion += 1;
      }}
      projects={projects.data ?? []}
      onsaved={saved}
    />
  {/key}
{/if}
