<script lang="ts">
  import type { Provider, ProviderProbe } from '$lib/api/management/providers';
  import { probeSummary } from './providerEditor';

  let {
    provider,
    probe,
    manualModelNames = $bindable(),
    busy,
    onDiscover,
    onDeclareModels
  }: {
    provider: Provider | null;
    probe: ProviderProbe | null;
    manualModelNames: string;
    busy: string;
    onDiscover: () => void | Promise<void>;
    onDeclareModels: () => void | Promise<void>;
  } = $props();

  const disabled = $derived(Boolean(busy));
</script>

<section class="card stage" aria-labelledby="discovery-heading">
  <p class="eyebrow">Probe passed</p>
  <h2 id="discovery-heading">Discover upstream models</h2>
  {#if probe}<p class="success-line">✓ {probeSummary(probe)}</p>{/if}
  <p>
    The connector will call the upstream model-list API with the stored
    identity. Discovered models begin disabled until their capabilities are
    certified and reviewed.
  </p>
  <button
    class="button button-primary"
    type="button"
    onclick={onDiscover}
    {disabled}
    >{busy === 'discover' ? 'Discovering…' : 'Discover upstream models'}</button
  >
  {#if provider?.kind === 'openai_compatible'}
    <details class="manual-fallback">
      <summary>Endpoint has no model-list API?</summary>
      <p>
        Declare identifiers manually. They remain disabled and capability-empty
        until you complete the same review.
      </p>
      <div class="form-field">
        <label for="manual-models-wizard">Upstream model identifiers</label>
        <textarea
          id="manual-models-wizard"
          bind:value={manualModelNames}
          placeholder="model-a&#10;model-b"></textarea>
      </div>
      <button
        class="button button-secondary"
        type="button"
        onclick={onDeclareModels}
        {disabled}
        >{busy === 'declare-models'
          ? 'Adding…'
          : 'Add identifiers for review'}</button
      >
    </details>
  {/if}
</section>

<style>
  .stage {
    max-width: 48rem;
    margin-top: 1.25rem;
    padding: clamp(1.15rem, 3vw, 1.75rem);
  }
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .stage > p {
    color: var(--foreground-muted);
  }
  .success-line {
    color: var(--success) !important;
    font-weight: 700;
  }
  .manual-fallback {
    margin-top: 1rem;
    padding: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
  }
  .manual-fallback summary {
    min-height: 2.75rem;
    font-weight: 720;
  }
  .manual-fallback p {
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .manual-fallback textarea {
    min-height: 5rem;
  }
</style>
