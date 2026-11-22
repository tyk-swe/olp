<script lang="ts">
  import type { CodePool } from '#lib/features/code-mode/api.js';

  let {
    items,
    allowed,
    busy,
    onEdit
  }: {
    items: CodePool[];
    allowed: boolean;
    busy: boolean;
    onEdit: (pool: CodePool) => void;
  } = $props();
</script>

{#each items as pool (pool.id)}
  <article class="card">
    <div class="actions">
      <h3>{pool.name}</h3>
      {#if allowed}<button
          class="button button-secondary"
          type="button"
          disabled={busy}
          onclick={() => onEdit(pool)}>Edit pool</button
        >{/if}
    </div>
    <dl>
      <dt>Pool</dt>
      <dd>{pool.id} · {pool.kind}</dd>
      <dt>Owner</dt>
      <dd>{pool.owner_user_id ?? 'Shared project pool'}</dd>
      <dt>Assigned accounts</dt>
      <dd>{pool.account_ids.join(', ') || 'None'}</dd>
      <dt>Explicit key permissions</dt>
      <dd>
        {pool.api_key_ids.join(', ') || 'None — no key can use this pool'}
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
  dd {
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
