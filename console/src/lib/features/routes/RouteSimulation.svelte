<script lang="ts">
  import RoutingDecisions from './RoutingDecisions.svelte';
  import type { RouteDraftEditorState } from '$lib/features/routes/routeDraftEditor.svelte';
  let { editor }: { editor: RouteDraftEditorState } = $props();
</script>

{#if editor.simulation}<section
    class="card simulation"
    aria-labelledby="simulation-heading"
  >
    <div class="section-heading">
      <div>
        <p class="eyebrow">Deterministic dry run</p>
        <h2 id="simulation-heading">Attempt explanation</h2>
      </div>
      <code>seed: {editor.simulation.deterministic_seed}</code>
    </div>
    <RoutingDecisions
      decisions={editor.simulation.targets.flatMap((target) =>
        target.decision ? [target.decision] : []
      )}
    />
  </section>{/if}

<style>
  .simulation {
    padding: 1.5rem;
    margin-top: 1rem;
  }
  .section-heading {
    display: flex;
    justify-content: space-between;
    gap: 1rem;
  }
  h2 {
    margin: 0.3rem 0 1rem;
  }
  code {
    font: 0.75rem monospace;
  }
</style>
