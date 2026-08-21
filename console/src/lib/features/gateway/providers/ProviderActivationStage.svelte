<script lang="ts">
  import { resolve } from '$app/paths';
  import type { Provider } from '$lib/api/management/providers';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import {
    activationReady,
    capabilitiesCertified,
    probeReady
  } from './providerEditor';

  let {
    provider,
    activated = false,
    busy,
    onTest,
    onActivate
  }: {
    provider: Provider | null;
    activated?: boolean;
    busy: string;
    onTest: () => void | Promise<void>;
    onActivate: () => void | Promise<void>;
  } = $props();

  const certified = $derived(capabilitiesCertified(provider));
  const tested = $derived(probeReady(provider));
  const ready = $derived(activationReady(provider));
  const disabled = $derived(Boolean(busy));
</script>

{#if activated}
  <section
    class="card stage complete-panel"
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
      <a
        class="button button-secondary"
        href={resolve(`/providers/${provider?.id}`)}>View provider</a
      >
    </div>
  </section>
{:else}
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
      Activation stays disabled until every tuple has server-owned certification
      and the completed draft passes a fresh connection test. Any configuration,
      credential, discovery, or capability change invalidates that evidence.
    </p>
  {/if}
{/if}

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
  .stage > p,
  .audit-note {
    color: var(--foreground-muted);
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
    font-size: 0.8rem;
  }
  .activation-checklist li {
    min-height: 1.5rem;
  }
  .activation-checklist li.complete {
    color: var(--success);
    font-weight: 700;
  }
  .complete-panel {
    text-align: center;
  }
  .complete-mark {
    display: grid;
    width: 3rem;
    height: 3rem;
    place-items: center;
    margin: 0 auto 1rem;
    border-radius: 50%;
    background: var(--success-soft);
    color: var(--success);
    font-size: 1.4rem;
    font-weight: 800;
  }
  .complete-panel .form-actions {
    justify-content: center;
  }
</style>
