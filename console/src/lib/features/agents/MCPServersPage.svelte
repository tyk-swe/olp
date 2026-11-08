<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { confirmDiscardChanges } from '#lib/editing/unsavedChanges.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { stateLabel } from '#lib/format.js';
  import { errorMessage } from '#lib/management/http.js';
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
  let editorOpen = $state(false);
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
    editorOpen = true;
  }
  async function saved(server: MCPServer) {
    client.setQueryData(agentKeys.server(server.id), server);
    selected = '';
    editorOpen = false;
    notify('Server certified and available to toolsets');
    await client.invalidateQueries({ queryKey: agentKeys.servers() });
  }
</script>

<svelte:head><title>MCP servers · OpenLLMProxy</title></svelte:head>
<PageHeader
  title="MCP servers"
  description="Register and certify MCP servers, then add their tools to toolsets."
>
  {#snippet actions()}
    {#if access.allows('POST /api/v1/mcp-servers')}
      <button
        class="button button-primary"
        type="button"
        onclick={() => select('')}>New server</button
      >
    {/if}
  {/snippet}
</PageHeader>
{#if projects.error}
  <Callout tone="danger">{errorMessage(projects.error)}</Callout>
{/if}

{#if servers.error}
  <Callout tone="danger">{errorMessage(servers.error)}</Callout>
{:else if servers.isPending}
  <div class="loading-state" role="status">Loading servers…</div>
{:else if !servers.data?.length}
  <EmptyState title="No MCP servers yet"
    >Register a server to make its tools available to agents.</EmptyState
  >
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
                  >{canEdit ? 'Edit' : 'View'}</button
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

{#if editorOpen && (!selected || current)}
  <Dialog
    title={current ? current.name : 'New MCP server'}
    placement="side"
    size="wide"
    canClose={() => confirmDiscardChanges(editor?.isDirty() ?? false)}
    onClose={() => {
      editorOpen = false;
      selected = '';
    }}
  >
    {#if current}
      <details class="stack-card">
        <summary>Certified tool catalog</summary>
        <pre class="code-block">{JSON.stringify(current.catalog, null, 2)}</pre>
      </details>
    {/if}
    {#if !current?.retired_at && access.allows(current ? 'PUT /api/v1/mcp-servers/{server_id}' : 'POST /api/v1/mcp-servers')}
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
  </Dialog>
{/if}
