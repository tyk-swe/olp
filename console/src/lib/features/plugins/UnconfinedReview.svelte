<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { pluginTitle, type UnconfinedExecutableReview } from './api';

  let {
    review,
    acknowledged = $bindable(),
    busy,
    permit,
    oncancel
  }: {
    review: UnconfinedExecutableReview;
    acknowledged: boolean;
    busy: string;
    permit: () => void;
    oncancel: () => void;
  } = $props();
</script>

<section class="review" aria-labelledby="unconfined-review-heading">
  <h3 id="unconfined-review-heading">Review {pluginTitle(review)}</h3>
  {#if review.manifest.description}<p>
      {review.manifest.description}
    </p>{/if}
  <dl class="facts">
    <div>
      <dt>Executable</dt>
      <dd><code>{review.name}</code></dd>
    </div>
    <div>
      <dt>Digest</dt>
      <dd><code class="digest">{review.digest}</code></dd>
    </div>
    <div>
      <dt>ABI</dt>
      <dd>{review.abi_version}</dd>
    </div>
  </dl>
  <ScrollTable label="Plugin profiles">
    <table class="data-table">
      <thead
        ><tr><th>Profile</th><th>Label</th><th>Dialect</th><th>Address</th></tr
        ></thead
      >
      <tbody>
        {#each review.manifest.profiles as profile (profile.id)}
          <tr
            ><td><code>{profile.id}</code></td><td>{profile.label}</td><td
              ><code>{profile.dialect}</code></td
            ><td><code class="address">{profile.hosting.address}</code></td></tr
          >
        {/each}
      </tbody>
    </table>
  </ScrollTable>
  <p class="muted">
    Declared origins:
    {#each review.manifest.origins as origin, index (origin)}{index
        ? ', '
        : ''}<code>{origin}</code>{:else}none{/each}.
  </p>
  <div class="risk" role="note">
    <strong>High risk.</strong> An unconfined plugin runs outside every confinement,
    with the operating system privileges of each OpenLLMProxy process that serves
    its providers. It receives their credentials and requests, can read whatever those
    processes can, reaches any network and bypasses the egress policy. Permit only
    an executable you trust as much as OpenLLMProxy itself.
  </div>
  <label class="acknowledge"
    ><input type="checkbox" bind:checked={acknowledged} /> I understand that {review.name}
    will run unconfined, with OpenLLMProxy's privileges.</label
  >
  <div class="actions">
    <button
      class="button button-primary"
      type="button"
      disabled={!acknowledged || Boolean(busy)}
      onclick={permit}
      >{busy === 'permit' ? 'Permitting…' : 'Permit unconfined plugin'}</button
    ><button
      class="button button-secondary"
      type="button"
      disabled={Boolean(busy)}
      onclick={oncancel}>Cancel</button
    >
  </div>
</section>

<style>
  .facts dt {
    margin: 0;
    color: var(--foreground-muted);
  }
  .review {
    display: grid;
    gap: 0.9rem;
    padding: 1rem;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-control);
  }
  .review p {
    margin: 0;
  }
  .facts {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr));
    gap: 1rem;
    margin: 0;
  }
  .facts dt {
    font-size: var(--text-caption);
  }
  .facts dd {
    margin: 0.25rem 0 0;
    min-width: 0;
  }
  .digest,
  .address {
    overflow-wrap: anywhere;
  }
  .risk {
    padding: 0.9rem 1rem;
    border: 1px solid var(--danger);
    border-radius: var(--radius-control);
    background: var(--danger-soft);
    color: var(--foreground);
  }
  .risk strong {
    color: var(--danger);
  }
  .acknowledge {
    display: flex;
    align-items: start;
    gap: 0.5rem;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 0.65rem;
  }
</style>
