<script lang="ts">
  import { resolve } from '$app/paths';
  import type { Provider } from '$lib/features/providers/api';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import {
    activationReady,
    capabilitiesCertified,
    probeReady
  } from '$lib/features/providers/providerEditor';

  let {
    provider,
    activated = false,
    busy,
    onBack,
    onTest,
    onActivate,
    onAddAnother
  }: {
    provider: Provider | null;
    activated?: boolean;
    busy: string;
    onBack?: () => void;
    onTest: () => void | Promise<void>;
    onActivate: () => void | Promise<void>;
    onAddAnother?: () => void;
  } = $props();

  const certified = $derived(capabilitiesCertified(provider));
  const tested = $derived(probeReady(provider));
  const ready = $derived(activationReady(provider));
  const disabled = $derived(Boolean(busy));
</script>

{#if activated}
  <section
    class="card card-light stage complete-panel"
    aria-labelledby="activated-heading"
  >
    <span class="complete-mark" aria-hidden="true">✓</span>
    <p class="eyebrow">Provider active</p>
    <h2 id="activated-heading">Now build a stable route slug.</h2>
    <p>
      {provider?.name} is eligible for new route drafts. Activation published an immutable
      runtime generation.
    </p>
    <div class="form-actions">
      <a class="button button-primary" href={resolve('/routes/new')}
        >Build default route <NavIcon name="arrow" /></a
      >
      {#if onAddAnother}<button
          class="button button-secondary"
          type="button"
          onclick={onAddAnother}>Add another connection</button
        >{/if}
      <a
        class="button button-secondary"
        href={resolve(`/providers/${provider?.id}`)}>View provider</a
      >
    </div>
  </section>
{:else}
  <section class="card stage" aria-labelledby="activation-heading">
    <p class="eyebrow">Activation</p>
    <h2 id="activation-heading">Activate the provider</h2>
    <p>
      Activation publishes an immutable runtime generation. Both requirements
      must hold for this exact draft.
    </p>
    <ol
      class="activation-checklist"
      aria-label="Provider activation requirements"
    >
      <li class:complete={certified}>
        {certified ? '✓' : '1'} Every enabled capability is server-certified
      </li>
      <li class:complete={tested}>
        {tested ? '✓' : '2'} Completed draft passed an ETag-bound connection test
      </li>
    </ol>
    <div class="form-actions">
      {#if onBack}<button
          class="button button-secondary"
          type="button"
          onclick={onBack}
          {disabled}>Back</button
        >{/if}
      <button
        class="button button-secondary"
        type="button"
        onclick={onTest}
        disabled={disabled || !certified}
        >{busy === 'final-probe'
          ? 'Testing completed draft…'
          : 'Test completed draft'}</button
      >
      <button
        class="button button-primary"
        type="button"
        onclick={onActivate}
        disabled={disabled || !ready}
        >{busy === 'activate' ? 'Activating…' : 'Activate provider'}</button
      >
    </div>
    {#if !ready}
      <p class="audit-note">
        Activation stays disabled until every tuple has server-owned
        certification and the completed draft passes a fresh connection test.
        Any configuration, credential, discovery, or capability change
        invalidates that evidence.
      </p>
    {/if}
  </section>
{/if}

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
  .stage > p:not(.eyebrow),
  .audit-note {
    color: var(--foreground-muted);
  }
  .audit-note {
    margin-top: 1rem;
    font-size: var(--text-body-sm);
  }
  .form-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
    margin-top: 1.35rem;
  }
  .activation-checklist {
    display: grid;
    gap: 0.4rem;
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .activation-checklist li {
    min-height: 1.5rem;
  }
  .activation-checklist li.complete {
    color: var(--success);
    font-weight: 500;
  }
  /* The one bright card in the wizard: `card-light` re-maps every token for
     the bone surface, so nothing below may assume a dark background. */
  .complete-panel {
    padding: 1.5rem;
    border-radius: var(--radius-panel);
    text-align: center;
  }
  .complete-panel h2 {
    font-size: 1.25rem;
    font-weight: 400;
  }
  .complete-mark {
    display: grid;
    width: 2.5rem;
    height: 2.5rem;
    place-items: center;
    margin: 0 auto 1rem;
    border: 1px solid var(--metric);
    border-radius: 50%;
    background: transparent;
    color: var(--metric);
    font-size: 1rem;
    font-weight: 500;
  }
  .complete-panel .form-actions {
    justify-content: center;
  }
</style>
