<script lang="ts">
  import { useQueryClient } from '@tanstack/svelte-query';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { stateLabel } from '#lib/format.js';
  import {
    approveServerCatalog,
    previewServerCatalog,
    type MCPCatalogPreview,
    type MCPServer
  } from './api';
  import { agentKeys } from './agentKeys';

  let {
    current,
    canApprove,
    onsaved
  }: {
    current: MCPServer;
    canApprove: () => boolean;
    onsaved: (server: MCPServer) => Promise<void>;
  } = $props();
  let review = $state<MCPCatalogPreview | null>(null);
  const client = useQueryClient();
  const preview = useAction();
  const approval = useMutation(async () => {
    review = null;
    await client.invalidateQueries({ queryKey: agentKeys.servers() });
  });
  const busy = $derived(preview.busy || approval.busy);
  const unchanged = $derived(
    review !== null &&
      review.discovered_digest === review.saved_digest &&
      review.discovered_protocol_version === review.saved_protocol_version
  );

  function runPreview() {
    return preview.run(
      () => previewServerCatalog(current.id),
      (value) => {
        approval.reset();
        review = value;
      }
    );
  }
  function runApproval() {
    if (!review || !canApprove()) return;
    return approval.submit(
      review,
      (input, requestId) => approveServerCatalog(input, requestId),
      async (server) => {
        await onsaved(server);
      }
    );
  }
  const schema = (value: unknown) => JSON.stringify(value, null, 2);
</script>

<section class="stack-card">
  <h3>Live catalog review</h3>
  <p>
    Rediscover the tools this server offers now and compare them with the
    certified catalog. Previewing never calls a tool and never changes the
    certified catalog.
  </p>
  <div class="page-actions">
    <button
      class="button button-secondary"
      type="button"
      disabled={busy}
      onclick={runPreview}
      >{review ? 'Preview again' : 'Preview live catalog'}</button
    >
  </div>
  {#if preview.busy}
    <p class="inline-status" role="status">Contacting the MCP server.</p>
  {/if}
  {#if preview.error}
    <p class="inline-problem" role="alert">{preview.error}</p>
  {/if}
  {#if review}
    <p class="inline-status">
      Certified revision {review.revision} uses protocol
      {review.saved_protocol_version} with digest
      <code>{review.saved_digest}</code>. The live server uses protocol
      {review.discovered_protocol_version} with digest
      <code>{review.discovered_digest}</code>.
    </p>
    {#if unchanged}
      <p>The live catalog matches the certified catalog.</p>
    {/if}
    {#if review.added.length}
      <h4>Added tools</h4>
      {#each review.added as tool (tool.name)}
        <details>
          <summary><code>{tool.name}</code></summary>
          <p>{tool.description}</p>
          <pre class="code-block">{schema(tool.input_schema)}</pre>
        </details>
      {/each}
    {/if}
    {#if review.removed.length}
      <h4>Removed tools</h4>
      {#each review.removed as tool (tool.name)}
        <details>
          <summary><code>{tool.name}</code></summary>
          <p>{tool.description}</p>
          <pre class="code-block">{schema(tool.input_schema)}</pre>
        </details>
      {/each}
    {/if}
    {#if review.modified.length}
      <h4>Modified tools</h4>
      {#each review.modified as tool (tool.name)}
        <details>
          <summary>
            <code>{tool.name}</code> changed
            {tool.changed_fields
              .map((field) => stateLabel(field))
              .join(' and ')}
          </summary>
          <p>Certified description: {tool.saved_description}</p>
          <p>Live description: {tool.discovered_description}</p>
          <p>Certified input schema</p>
          <pre class="code-block">{schema(tool.saved_input_schema)}</pre>
          <p>Live input schema</p>
          <pre class="code-block">{schema(tool.discovered_input_schema)}</pre>
        </details>
      {/each}
    {/if}
    {#if review.toolsets.length}
      <h4>Published toolsets pinning this server</h4>
      <p>
        Approval creates a new server revision. These toolsets serve this
        server's tools again only after they are republished.
      </p>
      <ul>
        {#each review.toolsets as toolset (toolset.id)}
          <li>{toolset.name} uses {toolset.tools.join(', ')}</li>
        {/each}
      </ul>
    {/if}
    {#if !unchanged}
      <div class="page-actions">
        <button
          class="button button-primary"
          type="button"
          disabled={busy}
          onclick={runApproval}>Approve and certify</button
        >
      </div>
    {/if}
  {/if}
  {#if approval.error}
    <p class="inline-problem" role="alert">{approval.error}</p>
  {/if}
</section>
