<script lang="ts">
  import { fidelityLabel } from '#lib/features/routes/routeFidelity.js';
  import type { RouteRevisionDiff } from './api.js';

  const { diff }: { diff: RouteRevisionDiff } = $props();
  const revisionDiff = $derived(diff);
</script>

<section class="diff-grid" aria-label="Revision differences">
  <article class="card">
    <p>Route settings</p>
    <strong>
      {[
        revisionDiff.slug_changed && 'slug',
        revisionDiff.timeout_changed && 'deadline',
        revisionDiff.max_attempts_changed && 'attempts',
        revisionDiff.fidelity_changed && 'fidelity',
        revisionDiff.routing_policy_changed && 'routing policy'
      ]
        .filter(Boolean)
        .join(', ') || 'unchanged'}
    </strong>
  </article>
  {#if revisionDiff.fidelity_changed}
    <article class="card">
      <p>Fidelity</p>
      <strong>
        {fidelityLabel(revisionDiff.fidelity_before)} →
        {fidelityLabel(revisionDiff.fidelity_after)}
      </strong>
      <p>
        Review client, continuation, mutation-policy, and state obligations
        before using the changed route.
      </p>
    </article>
  {/if}
  <article class="card">
    <p>Operations added</p>
    {#if revisionDiff.operations_added.length}<ul>
        {#each revisionDiff.operations_added as item (item)}<li>
            <code>{item}</code>
          </li>{/each}
      </ul>{:else}<strong>None</strong>{/if}
    <p>Operations removed</p>
    {#if revisionDiff.operations_removed.length}<ul>
        {#each revisionDiff.operations_removed as item (item)}<li>
            <code>{item}</code>
          </li>{/each}
      </ul>{:else}<strong>None</strong>{/if}
  </article>
  <article class="card">
    <p>Target changes</p>
    {#if revisionDiff.targets_added.length}<strong>Added</strong>
      <ul>
        {#each revisionDiff.targets_added as item (item)}<li>
            <code>{item}</code>
          </li>{/each}
      </ul>{/if}{#if revisionDiff.targets_removed.length}<strong>
        Removed
      </strong>
      <ul>
        {#each revisionDiff.targets_removed as item (item)}<li>
            <code>{item}</code>
          </li>{/each}
      </ul>{/if}{#if revisionDiff.targets_changed.length}<strong>
        Changed
      </strong>
      <ul>
        {#each revisionDiff.targets_changed as item (item)}<li>
            <code>{item}</code>
          </li>{/each}
      </ul>{/if}{#if !revisionDiff.targets_added.length && !revisionDiff.targets_removed.length && !revisionDiff.targets_changed.length}<strong>
        None
      </strong>{/if}
  </article>
  {#if revisionDiff.routing_policy_changed}<article class="card policy-diff">
      <p>Routing policy changes</p>
      <details open>
        <summary>Before · Revision {revisionDiff.from_revision}</summary>
        <pre>{JSON.stringify(revisionDiff.routing_policy_before, null, 2)}</pre>
      </details>
      <details open>
        <summary>After · Revision {revisionDiff.to_revision}</summary>
        <pre>{JSON.stringify(revisionDiff.routing_policy_after, null, 2)}</pre>
      </details>
    </article>{/if}
</section>

<style>
  .policy-diff {
    grid-column: 1 / -1;
  }
  .diff-grid {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 0.75rem;
    margin-bottom: 1rem;
  }
  .diff-grid article {
    min-width: 0;
    padding: 1.25rem;
  }
  .diff-grid p {
    margin: 0;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .diff-grid p:not(:first-child) {
    margin-top: 0.8rem;
  }
  .diff-grid strong {
    display: block;
    margin-top: 0.25rem;
  }
  .diff-grid ul {
    margin: 0.3rem 0 0;
    padding-left: 1rem;
  }
  .diff-grid li {
    overflow-wrap: anywhere;
  }
</style>
