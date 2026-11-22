<script lang="ts">
  import type { RequestDetail } from '#lib/features/usage/history/api.js';
  import AttemptCard from './AttemptCard.svelte';

  let { data }: { data: RequestDetail } = $props();
</script>

<section class="timeline-section" aria-labelledby="attempts-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Upstream</p>
      <h2 id="attempts-title">Attempt timeline</h2>
    </div>
    <span class="badge">{data.attempt_count} attempts</span>
  </div>
  {#if data.attempts.length === 0}
    <div class="card empty-state">No attempt metadata was recorded.</div>
  {:else}
    {#if data.attempts.length < data.attempt_count}
      <p class="partial-attempts" role="status">
        Showing {data.attempts.length} of {data.attempt_count} attempts. The rest
        were not recorded or have been retained out.
      </p>
    {/if}
    <ol class="timeline">
      {#each data.attempts as attempt (attempt.id)}
        <AttemptCard {attempt} />
      {/each}
    </ol>
  {/if}
</section>

<style>
  .partial-attempts {
    margin: 0.75rem 0 0;
    color: var(--warning);
    font-size: var(--text-caption);
  }
  .timeline-section {
    margin-top: 2rem;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .timeline {
    display: grid;
    gap: 0.75rem;
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
  }
</style>
