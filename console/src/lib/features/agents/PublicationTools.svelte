<script lang="ts">
  import { resolve } from '$app/paths';
  import type { PublicationForm, ServerPricingForm } from './form.js';
  import { listServers, type MCPServer } from './api.js';

  let {
    form = $bindable(),
    server = $bindable(),
    serverID = $bindable(),
    pricing = $bindable(),
    servers,
    busy,
    canPrice,
    onChooseServer,
    onSavePricing,
    onAddTool
  }: {
    form: PublicationForm;
    server: MCPServer | null;
    serverID: string;
    pricing: ServerPricingForm | null;
    servers: Awaited<ReturnType<typeof listServers>>;
    busy: boolean;
    canPrice: boolean;
    onChooseServer: () => void;
    onSavePricing: () => void;
    onAddTool: (name: string) => void;
  } = $props();

  function classifyTool(alias: string, value: string) {
    const classification =
      value === 'read_only' || value === 'approval_required'
        ? value
        : undefined;
    form.tools = form.tools.map((tool) =>
      tool.alias === alias ? { ...tool, classification } : tool
    );
  }
</script>

<a class="text-button" href={resolve('mcp-servers')}
  >Register or manage MCP servers</a
>

<label class="form-field">
  Certified MCP server

  <select
    aria-label="Certified MCP server"
    bind:value={serverID}
    onchange={onChooseServer}
    ><option value="">Choose a server</option
    >{#each servers.filter((value) => value.project_id === form.project && value.enabled) as value (value.id)}<option
        value={value.id}>{value.name}</option
      >{/each}</select
  ></label
>
{#if server}<p>
    Transport <code>{server.transport}</code>, MCP
    <code>{server.catalog.protocol_version}</code>
  </p>
  <p>Catalog <code>{server.catalog.digest}</code></p>
  {#each server.catalog.tools as tool (tool.name)}<button
      class="button button-secondary"
      type="button"
      onclick={() => onAddTool(tool.name)}
      disabled={form.tools.length >= 128 ||
        form.tools.some(
          (value) => value.server_id === server?.id && value.name === tool.name
        )}>Add {tool.name}</button
    >{/each}{/if}
<ul>
  {#each form.tools as tool (tool.alias)}<li>
      {tool.name} <code>{tool.revision_id}</code>
      <label class="form-field">
        Execution classification
        <select
          aria-label={`Execution classification for ${tool.name}`}
          value={tool.classification ?? ''}
          onchange={(event) =>
            classifyTool(tool.alias, event.currentTarget.value)}
        >
          <option value="">Unclassified</option>
          <option value="read_only">Read-only</option>
          <option value="approval_required">Approval required</option>
        </select>
      </label><button
        class="button button-secondary"
        type="button"
        onclick={() =>
          (form.tools = form.tools.filter(
            (value) => value.alias !== tool.alias
          ))}>Remove</button
      >
    </li>
  {/each}
</ul>
<p>
  Each selected tool pins its certified server revision; catalog drift revokes
  its live authority. Only tools explicitly marked read-only can run
  automatically in a gateway workflow.
</p>
{#if server && pricing}
  <fieldset>
    <legend>Tool pricing and limits</legend>
    <label class="form-field"
      >Server unit price (blank unpriced, 0 free) <input
        bind:value={pricing.unitPrice}
        inputmode="decimal"
      /></label
    >
    {#each server.catalog.tools as tool (tool.name)}
      <label class="form-field"
        >{tool.name} unit price
        <input
          bind:value={pricing.toolPrices[tool.name]}
          inputmode="decimal"
        /></label
      >
    {/each}
    <label class="form-field"
      >Server calls per minute <input
        bind:value={pricing.callsPerMinute}
        inputmode="numeric"
      /></label
    ><label class="form-field"
      >Server max concurrency <input
        bind:value={pricing.maxConcurrency}
        inputmode="numeric"
      /></label
    ><label class="form-field"
      >Per-key calls per minute <input
        bind:value={pricing.perKeyCallsPerMinute}
        inputmode="numeric"
      /></label
    ><label class="form-field"
      >Per-key max concurrency <input
        bind:value={pricing.perKeyMaxConcurrency}
        inputmode="numeric"
      /></label
    >
    <button
      class="button button-secondary"
      type="button"
      disabled={busy || !canPrice}
      onclick={() => void onSavePricing()}>Save pricing</button
    >
    <p>
      Prices are in the installation pricing currency and pin with the certified
      server revision. Saving recertifies the server, so republish toolsets to
      pin the new revision.
    </p>
  </fieldset>
{/if}
