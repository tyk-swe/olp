<script lang="ts">
  import { tokenCount } from './presentation';
  import type { CodeBudget } from '#lib/features/code-mode/api.js';

  let {
    items,
    allowed,
    busy,
    onEdit
  }: {
    items: CodeBudget[];
    allowed: boolean;
    busy: boolean;
    onEdit: (budget: CodeBudget) => void;
  } = $props();
</script>

<p>
  Daily and monthly periods use UTC. These are token caps, never subscription
  allowance or billed spend. Existing key request/rate/concurrency limits also
  apply. Paid API targets additionally require known pricing and monetary
  admission.
</p>
{#each items as budget (budget.id)}
  <article class="card">
    <div class="actions">
      <h3>Token budget</h3>
      {#if allowed}<button
          class="button button-secondary"
          type="button"
          disabled={busy}
          onclick={() => onEdit(budget)}>Edit budget</button
        >{/if}
    </div>
    <dl>
      <dt>Budget</dt>
      <dd>{budget.id} · {budget.enabled ? 'enabled' : 'disabled'}</dd>
      <dt>Route / key scope</dt>
      <dd>
        {budget.route_id ?? 'All project routes'} / {budget.api_key_id ??
          'All project keys'}
      </dd>
      <dt>Daily tokens</dt>
      <dd>
        {budget.daily_tokens == null
          ? 'No cap'
          : tokenCount(budget.daily_tokens)}
      </dd>
      <dt>Monthly tokens</dt>
      <dd>
        {budget.monthly_tokens == null
          ? 'No cap'
          : tokenCount(budget.monthly_tokens)}
      </dd>
    </dl>
  </article>
{/each}

<style>
  .actions {
    display: flex;
    gap: 0.65rem;
    align-items: center;
    flex-wrap: wrap;
  }
  h3 {
    flex: 1;
    font-weight: 600;
  }
  article {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  dl {
    display: grid;
    grid-template-columns: minmax(8rem, 1fr) 3fr;
    gap: 0.6rem 1rem;
  }
  dt {
    font-weight: 500;
  }
  dd,
  p {
    color: var(--foreground-subtle);
    line-height: 1.6;
    overflow-wrap: anywhere;
  }
  @media (max-width: 40rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
