<script lang="ts">
  import type { RouteDraftEditorState } from './routeDraftEditor.svelte';
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
    <ol>
      {#each editor.simulation.targets as target (target.target_id)}<li
          class:ineligible={!target.eligible}
        >
          <span class="attempt">{target.attempt ?? '—'}</span>
          <div>
            <strong>{target.provider_name} · {target.provider_model}</strong>
            <p>
              {target.eligible
                ? `Eligible in priority group ${target.priority}`
                : (target.reason ?? 'Capability tuple is not eligible')}
            </p>
          </div>
          <span
            class:success={target.eligible}
            class:warning={!target.eligible}
            class="badge">{target.eligible ? 'eligible' : 'filtered'}</span
          >
        </li>{/each}
    </ol>
  </section>{/if}

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }

  .simulation {
    padding: clamp(1.1rem, 2.5vw, 1.5rem);
  }

  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }

  .simulation {
    margin-top: 1rem;
  }
  .simulation ol {
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
  }
  .simulation li {
    display: grid;
    grid-template-columns: 2.2rem 1fr auto;
    align-items: center;
    gap: 0.75rem;
    min-height: 4rem;
    border-top: 1px solid var(--border);
  }
  .simulation li.ineligible {
    color: var(--foreground-muted);
  }
  .simulation p {
    margin: 0.15rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .attempt {
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    border-radius: 50%;
    background: var(--surface-subtle);
    font: 700 0.72rem 'JetBrains Mono Variable';
  }

  code {
    font:
      0.7rem 'JetBrains Mono Variable',
      monospace;
  }
</style>
