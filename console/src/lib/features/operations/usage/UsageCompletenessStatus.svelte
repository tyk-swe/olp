<script lang="ts">
  import { resolve } from '$app/paths';
  import type { UsageCompleteness } from '$lib/api/usage';
  import { presentUsageCompleteness } from './completenessPresentation';

  let { completeness }: { completeness: UsageCompleteness } = $props();
  const presentation = $derived(presentUsageCompleteness(completeness));
</script>

{#if presentation.kind === 'warning'}
  <section
    class="completeness"
    class:danger={presentation.danger}
    role="status"
    aria-labelledby="completeness-title"
  >
    <div>
      <strong id="completeness-title">{presentation.title}</strong>
      <p>{presentation.detail}</p>
    </div>
    <a href={resolve('/health')}>Open health</a>
  </section>
{:else}
  <p class="complete-banner">
    <span aria-hidden="true">✓</span> Usage accounting and pricing are complete for
    this range.
  </p>
{/if}

<style>
  .completeness {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    margin-top: 1.25rem;
    padding: 0.9rem 1rem;
    border: 1px solid color-mix(in srgb, var(--warning) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--warning-soft);
    color: var(--warning);
  }
  .completeness.danger {
    border-color: var(--danger);
    background: var(--danger-soft);
    color: var(--danger);
  }
  .completeness p {
    margin: 0.2rem 0 0;
  }
  .completeness a {
    display: inline-flex;
    min-height: 2.75rem;
    flex: none;
    align-items: center;
    font-weight: 700;
  }
  .complete-banner {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 1.25rem 0 0;
    color: var(--success);
    font-weight: 700;
  }
  @media (max-width: 40rem) {
    .completeness {
      display: grid;
      align-items: flex-start;
    }
  }
</style>
