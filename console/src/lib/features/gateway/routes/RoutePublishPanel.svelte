<script lang="ts">
  import { resolve } from '$app/paths';
  import { modesFor, surfacesFor } from './routeEditor';
  import type { RouteDraftEditorState } from './routeDraftEditor.svelte';
  let { editor }: { editor: RouteDraftEditorState } = $props();
</script>

<aside class="card publish-panel" aria-labelledby="publish-heading">
  <p class="eyebrow">Draft controls</p>
  <h2 id="publish-heading">Test before activation</h2>
  <p>
    Saving changes invalidates prior validation. Unsaved edits must be saved
    before you can simulate or validate them.
  </p>
  <button
    class="button button-secondary"
    type="submit"
    disabled={!editor.canManage || Boolean(editor.busy)}
    >{editor.busy === 'save'
      ? 'Saving…'
      : editor.isNew
        ? 'Create draft'
        : 'Save draft'}</button
  >
  {#if !editor.isNew && editor.draft.data}
    <hr />
    <label for="simulation-operation">Dry-run operation</label>
    <select id="simulation-operation" bind:value={editor.simulationOperation}
      >{#each editor.operations as operation (operation)}<option
          value={operation}>{operation}</option
        >{/each}</select
    >
    <label for="simulation-surface">Client surface</label>
    <select id="simulation-surface" bind:value={editor.simulationSurface}
      >{#each surfacesFor(editor.simulationOperation) as surface (surface)}<option
          value={surface}>{surface}</option
        >{/each}</select
    >
    <label for="simulation-mode">Transport mode</label>
    <select id="simulation-mode" bind:value={editor.simulationMode}
      >{#each modesFor(editor.simulationOperation) as mode (mode)}<option
          value={mode}>{mode}</option
        >{/each}</select
    >
    <label for="simulation-seed">Dry-run seed</label>
    <input id="simulation-seed" bind:value={editor.seed} />
    <button
      class="button button-secondary"
      type="button"
      onclick={() => editor.simulate(editor.draft.data!)}
      disabled={!editor.canManage || Boolean(editor.busy) || editor.sync.dirty}
      >{editor.busy === 'simulate' ? 'Simulating…' : 'Simulate order'}</button
    >
    <button
      class="button button-secondary"
      type="button"
      onclick={() => editor.validate(editor.draft.data!)}
      disabled={!editor.canManage || Boolean(editor.busy) || editor.sync.dirty}
      >{editor.busy === 'validate' ? 'Validating…' : 'Validate draft'}</button
    >
    <button
      class="button button-primary"
      type="button"
      onclick={() => editor.activate(editor.draft.data!)}
      disabled={!editor.canManage || Boolean(editor.busy) || editor.sync.dirty}
      >{editor.busy === 'activate' ? 'Activating…' : 'Activate route'}</button
    >
  {/if}
  {#if editor.activation}<div class="activation">
      <strong>Revision {editor.activation.revision} active</strong><span
        >Runtime generation {editor.activation.runtime_generation
          .sequence}</span
      ><a href={resolve(`/routes/${editor.activation.route_id}/revisions`)}
        >View revision history</a
      >
    </div>{/if}
</aside>

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }

  .publish-panel {
    padding: clamp(1.1rem, 2.5vw, 1.5rem);
  }

  .publish-panel p {
    color: var(--foreground-muted);
  }
  .publish-panel {
    position: sticky;
    top: 5rem;
    display: grid;
    gap: 0.65rem;
  }
  .publish-panel h2,
  .publish-panel p {
    margin-bottom: 0;
  }
  .publish-panel hr {
    width: 100%;
    margin: 0.5rem 0;
    border: 0;
    border-top: 1px solid var(--border);
  }
  .publish-panel > :is(input, select) {
    min-height: 2.5rem;
    padding: 0.5rem 0.65rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .activation {
    display: grid;
    gap: 0.2rem;
    padding: 0.75rem;
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
    font-size: 0.78rem;
  }
  .activation a {
    min-height: 2.75rem;
    padding-top: 0.65rem;
    font-weight: 750;
  }

  @media (max-width: 76rem) {
    .publish-panel {
      position: static;
      grid-template-columns: repeat(3, 1fr);
    }
    .publish-panel
      > :is(.eyebrow, h2, p, hr, label, input, select, .activation) {
      grid-column: 1 / -1;
    }
  }
  @media (max-width: 48rem) {
    .publish-panel {
      grid-template-columns: 1fr;
    }
  }
</style>
