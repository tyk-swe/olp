<script lang="ts">
  import RoutingDecisions from './RoutingDecisions.svelte';
  import {
    decisionRows,
    simulationRows
  } from '$lib/features/routes/routingExplanation';
  import type { RouteDraftEditorState } from '$lib/features/routes/routeDraftEditor.svelte';
  let { editor }: { editor: RouteDraftEditorState } = $props();
  const words = (code: string) => code.replaceAll('_', ' ');
</script>

{#if editor.simulation}<section
    class="card simulation"
    aria-labelledby="simulation-heading"
  >
    <div class="section-heading">
      <div>
        <p class="eyebrow">Plan only · no provider request</p>
        <h2 id="simulation-heading">Attempt explanation</h2>
      </div>
      <code
        >{editor.simulation.operation} · {editor.simulation.surface} · {editor
          .simulation.mode} · seed: {editor.simulation.deterministic_seed}</code
      >
    </div>
    {#if editor.simulation.selectors.length}<ol
        class="trace"
        aria-label="Selectors evaluated"
      >
        {#each editor.simulation.selectors as outcome (outcome.id)}<li
            class:matched={outcome.matched}
          >
            <code>{outcome.id}</code>
            {words(outcome.outcome)}{#if outcome.label}
              · label <code>{outcome.label}</code>{/if}
          </li>{/each}
      </ol>{/if}
    {#if editor.simulation.affinity}<p class="muted">
        Session affinity by {words(
          editor.simulation.affinity.source
        )}{#if editor.simulation.affinity.label}
          <code>{editor.simulation.affinity.label}</code>{/if}:
        {editor.simulation.affinity.session
          ? 'the sample request carries a session, so its attempts stay on one target and slot.'
          : 'the sample request carries no session key, so the seed orders targets.'}
      </p>{/if}
    <RoutingDecisions rows={simulationRows(editor.simulation.targets)} />
    {#each editor.simulation.legs as leg, index (index)}
      <h3>
        {leg.via === 'selector' ? 'Delegated to' : 'Falls back to'}
        <code>{leg.route}</code>
      </h3>
      <RoutingDecisions rows={decisionRows(leg.decisions)} />
    {/each}
    {#if editor.simulation.fallbacks.length}<h3>Fallback routes</h3>
      <ul class="fallbacks">
        {#each editor.simulation.fallbacks as step, index (index)}<li>
            <code>{step.from}</code> → <code>{step.route}</code> on
            {step.conditions.map(words).join(', ')}:
            <strong>{words(step.outcome)}</strong>
          </li>{/each}
      </ul>{/if}
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
  h3 {
    margin: 1.2rem 0 0.6rem;
    font-size: var(--text-body);
    font-weight: 500;
  }
  .trace,
  .fallbacks {
    margin: 0 0 1rem;
    padding-left: 1.2rem;
    display: grid;
    gap: 0.25rem;
  }
  .trace .matched {
    color: var(--signal);
  }
  .muted {
    color: var(--foreground-subtle);
    font-size: var(--text-caption);
  }
  code {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
</style>
