<script lang="ts">
  import { errorMessage } from '$lib/api/http';
  import RunLimits from '$lib/features/workflows/RunLimits.svelte';
  import type { RouteDraftEditorState } from './routeDraftEditor.svelte';

  let { editor }: { editor: RouteDraftEditorState } = $props();

  const selected = $derived(editor.behavior.toolsets ?? []);
  const project = $derived(
    editor.isNew ? editor.projectId : (editor.draft.data?.project_id ?? '')
  );
  const available = $derived(
    (editor.toolsets.data ?? []).filter(
      (toolset) =>
        !toolset.retired &&
        toolset.revision_id &&
        (!project || toolset.project_id === project)
    )
  );
  const unavailable = $derived(
    selected.filter((id) => !available.some((toolset) => toolset.id === id))
  );

  function setExecution(value: string) {
    editor.behavior = {
      ...editor.behavior,
      tool_execution: value === 'gateway' ? 'gateway' : '',
      toolsets: value === 'gateway' ? selected : [],
      run_limits: value === 'gateway' ? editor.behavior.run_limits : null,
      responses_mcp_items:
        value === 'gateway' && editor.behavior.responses_mcp_items
    };
    editor.touch();
  }

  function selectToolset(id: string, checked: boolean) {
    editor.behavior = {
      ...editor.behavior,
      toolsets: checked
        ? [...selected, id]
        : selected.filter((value) => value !== id)
    };
    editor.touch();
  }
</script>

<section class="card workflow-editor" aria-labelledby="route-workflow-heading">
  <p class="eyebrow">Agent workflows</p>
  <h2 id="route-workflow-heading">Tool execution</h2>
  <div class="form-field">
    <label for="route-tool-execution">Who executes tools?</label>
    <select
      id="route-tool-execution"
      value={editor.behavior.tool_execution ?? ''}
      onchange={(event) => setExecution(event.currentTarget.value)}
      disabled={!editor.canManage || Boolean(editor.busy)}
    >
      <option value="">Client</option>
      <option value="gateway">Gateway</option>
    </select>
    <small>
      Gateway execution uses published read-only tools on transformed inference
      routes. Callers can choose a subset of the allowed toolsets.
    </small>
  </div>
  {#if editor.behavior.tool_execution === 'gateway'}
    <RunLimits
      prefix="route-run"
      value={editor.behavior.run_limits}
      disabled={!editor.canManage || Boolean(editor.busy)}
      onChange={(run_limits) => {
        editor.behavior = { ...editor.behavior, run_limits };
        editor.touch();
      }}
    />
    <label>
      <input
        type="checkbox"
        checked={editor.behavior.responses_mcp_items ?? false}
        onchange={(event) => {
          editor.behavior = {
            ...editor.behavior,
            responses_mcp_items: event.currentTarget.checked
          };
          editor.touch();
        }}
        disabled={!editor.canManage || Boolean(editor.busy)}
      />
      Responses clients support MCP output items
    </label>
    {#if editor.fidelity.mode !== 'transformed'}
      <p class="inline-problem" role="alert">
        Choose transformed fidelity for gateway tool execution.
      </p>
    {/if}
    {#if editor.toolsets.isError}
      <p class="inline-problem" role="alert">
        Could not load toolsets. {errorMessage(editor.toolsets.error)}
        <button type="button" onclick={() => editor.toolsets.refetch()}
          >Retry</button
        >
      </p>
    {/if}
    <fieldset disabled={!editor.canManage || Boolean(editor.busy)}>
      <legend>Allowed published toolsets</legend>
      {#if editor.toolsets.isPending}
        <p role="status">Loading published toolsets…</p>
      {:else if available.length === 0 && unavailable.length === 0}
        <p>No published toolsets are available for this project.</p>
      {/if}
      {#each available as toolset (toolset.id)}
        <label class="toolset">
          <input
            type="checkbox"
            checked={selected.includes(toolset.id)}
            onchange={(event) =>
              selectToolset(toolset.id, event.currentTarget.checked)}
          />
          <span>{toolset.name} <code>{toolset.id}</code></span>
        </label>
      {/each}
      {#each unavailable as id (id)}
        <label class="toolset">
          <input
            type="checkbox"
            checked
            onchange={(event) => selectToolset(id, event.currentTarget.checked)}
          />
          <span>Unavailable toolset <code>{id}</code></span>
        </label>
      {/each}
    </fieldset>
  {/if}
</section>

<style>
  .workflow-editor {
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 1rem;
    font-size: 1rem;
    font-weight: 500;
  }
  fieldset {
    margin-top: 1rem;
  }
  .toolset {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    min-height: 2.75rem;
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
</style>
