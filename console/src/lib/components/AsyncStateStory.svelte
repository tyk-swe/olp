<script lang="ts">
  let { state }: { state: 'loading' | 'empty' | 'error' } = $props();
</script>

<main>
  <p class="eyebrow">Request Explorer</p>
  <h1>Operational state</h1>
  {#if state === 'loading'}
    <div class="loading-state" role="status">Loading request metadata…</div>
  {:else if state === 'empty'}
    <section class="card empty-state" aria-labelledby="empty-heading">
      <h2 id="empty-heading">No requests match these filters</h2>
      <p>Clear one or more filters to expand the result set.</p>
    </section>
  {:else}
    <div class="inline-problem" role="alert">
      <strong>Request metadata is temporarily unavailable.</strong>
      <span>The gateway can continue serving inference from its last-known-good runtime.</span>
      <button class="button button-secondary" type="button">Retry</button>
    </div>
  {/if}
</main>

<style>
  main { width: min(46rem, calc(100vw - 2rem)); padding: 2rem 1rem; }
  h1, h2 { margin: 0; }
  .loading-state, .empty-state, .inline-problem { margin-top: 1.25rem; }
  .empty-state { padding: 1.25rem; }
  .empty-state p { margin-bottom: 0; color: var(--foreground-muted); }
  .inline-problem { display: grid; gap: .65rem; }
  .inline-problem .button { justify-self: start; }
</style>
