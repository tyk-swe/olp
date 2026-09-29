<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import type { Provider, ProviderProbe } from '$lib/features/providers/api';
  import {
    declaresModels,
    listProviderProfiles
  } from '$lib/features/providers/profiles';
  import { probeSummary } from '$lib/features/providers/providerEditor';
  import { providerKeys } from '$lib/features/providers/providerKeys';

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
  const profiles = createQuery(() => ({
    queryKey: providerKeys.profiles(),
    queryFn: ({ signal }) => listProviderProfiles(signal)
  }));
  // A plugin profile without model discovery has no upstream model list: the
  // operator declares models and each is certified.
  const declared = $derived(
    provider ? declaresModels(provider.configuration, profiles.data) : false
  );
</script>

{#snippet declaration()}
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
{/snippet}

<section class="card stage" aria-labelledby="discovery-heading">
  <p class="eyebrow">Probe passed</p>
  {#if declared}
    <h2 id="discovery-heading">Declare upstream models</h2>
    {#if probe}<p class="success-line">✓ {probeSummary(probe)}</p>{/if}
    <p>
      This plugin profile declares no upstream model list. The probe model is
      already declared; add any other model identifiers here. Each model is
      certified individually before it can serve.
    </p>
    <div class="manual-fallback">{@render declaration()}</div>
  {:else}
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
      >{busy === 'discover'
        ? 'Discovering…'
        : 'Discover upstream models'}</button
    >
    {#if provider?.configuration.kind === 'openai_compatible'}
      <details class="manual-fallback">
        <summary>Endpoint has no model-list API?</summary>
        <p>
          Declare identifiers manually. They remain disabled and
          capability-empty until you complete the same review.
        </p>
        {@render declaration()}
      </details>
    {/if}
  {/if}
</section>

<style>
  .stage {
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .stage > p:not(.eyebrow) {
    color: var(--foreground-muted);
  }
  .stage > .success-line {
    color: var(--success);
    font-weight: 500;
  }
  .manual-fallback {
    margin-top: 1rem;
    padding: 0.75rem;
    border: 0;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .manual-fallback summary {
    min-height: 2.75rem;
    font-weight: 500;
  }
  .manual-fallback p {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .manual-fallback textarea {
    min-height: 5rem;
  }
</style>
