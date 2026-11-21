<script lang="ts">
  import { formatDate } from '#lib/format.js';
  import type { CodeBinding } from '#lib/features/code-mode/api.js';

  let {
    items,
    allowed,
    retiringKey,
    onRetire
  }: {
    items: CodeBinding[];
    allowed: boolean;
    retiringKey: string | null;
    onRetire: (item: CodeBinding) => void;
  } = $props();
</script>

<p>
  A tree pins an account to each model at its first use and holds at most one
  account of each subscription. Parents, children, resumes, reconnects and
  compaction stay on that model's account. Recovery can change the account only
  after proven nonexecution and safe continuation checks. Revoked permissions
  still refuse access. Retirement is permanent for the entire tree
</p>
{#each items as item (item.id)}
  <article class="card">
    <h3>
      {item.retired_at ? 'Retired tree binding' : 'Pinned tree binding'}
    </h3>
    <dl>
      <dt>Conversation</dt>
      <dd>{item.conversation}</dd>
      <dt>Binding / root</dt>
      <dd>{item.id} / {item.root_id}</dd>
      <dt>Parent binding</dt>
      <dd>{item.parent_id ?? 'Root conversation'}</dd>
      <dt>First target</dt>
      <dd>
        {item.paid_provider_id
          ? 'Paid API ' + item.paid_provider_id + ' / ' + item.paid_slot_id
          : item.account_id + ' / ' + item.principal}
      </dd>
      <dt>Model pins</dt>
      <dd>
        {item.pins
          .map(
            (pin) => `${pin.model} -> ${pin.paid_provider_id ?? pin.account_id}`
          )
          .join(', ') || 'No model served yet'}
      </dd>
      <dt>Route / key</dt>
      <dd>{item.route_id} / {item.api_key_id}</dd>
      <dt>Created / retired</dt>
      <dd>
        {formatDate(item.created_at)} / {item.retired_at
          ? formatDate(item.retired_at)
          : 'Not retired'}
      </dd>
    </dl>
    {#if allowed && !item.retired_at}<button
        type="button"
        class="button button-secondary button-danger"
        disabled={!!retiringKey}
        onclick={() => onRetire(item)}
        >{retiringKey === item.id ? 'Retiring…' : 'Retire entire tree'}</button
      >{/if}
  </article>
{/each}

<style>
  article {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  h3 {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  dl {
    display: grid;
    grid-template-columns: minmax(8rem, 1fr) 3fr;
    gap: 0.6rem 1rem;
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
