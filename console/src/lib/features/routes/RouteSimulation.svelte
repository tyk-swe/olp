<script lang="ts">
  import RoutingDecisions from './RoutingDecisions.svelte';
  import { simulationRows } from '$lib/features/routes/routingExplanation';
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
      <code
        >{editor.simulation.operation} · {editor.simulation.surface} · {editor
          .simulation.mode} · seed: {editor.simulation.deterministic_seed}</code
      >
    </div>
    <RoutingDecisions rows={simulationRows(editor.simulation.targets)} />
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
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  code {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
</style>
