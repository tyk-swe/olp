<script lang="ts">
  import { untrack } from 'svelte';
  import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
  import type { Provider } from './api';
  import { updateProvider } from './api';
  import { errorMessage } from '$lib/api/http';
  let {
    provider,
    canManage,
    onChanged
  }: {
    provider: Provider;
    canManage: boolean;
    onChanged: () => void | Promise<void>;
  } = $props();
  let text = $state('');
  let baseline = $state<Provider | null>(null);
  let dirty = $state(false);
  let error = $state('');
  let busy = $state(false);
  function hydrate(current: Provider) {
    baseline = current;
    text = JSON.stringify(current.configuration.options ?? {}, null, 2);
    dirty = false;
    error = '';
  }
  $effect(() => {
    const current = provider;
    untrack(() => {
      if (!dirty || baseline?.id !== current.id) hydrate(current);
    });
  });
  guardUnsavedChanges(() => dirty);
  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (!baseline || !canManage || busy) return;
    busy = true;
    error = '';
    try {
      const updated = await updateProvider(baseline.id, baseline.etag, {
        name: baseline.name,
        configuration: { ...baseline.configuration, options: JSON.parse(text) }
      });
      hydrate(updated);
      await onChanged();
    } catch (e) {
      error = errorMessage(e, 'Enter valid connection options.');
    } finally {
      busy = false;
    }
  }
</script>

<details class="card options">
  <summary>Vendor options and model metadata</summary>
  <p>
    Configure vendor identity, parameter defaults, and model facts used by
    routing. Privacy declarations require a source and observation time.
    Credential values belong in the encrypted credential pool.
  </p>
  {#if error}<p role="alert" class="inline-problem">{error}</p>{/if}
  {#if dirty && baseline?.etag !== provider.etag}<p role="status">
      This provider changed while you were editing. Your options are preserved.
    </p>{/if}
  {#if dirty || error}<button
      type="button"
      class="button button-secondary"
      disabled={busy}
      onclick={() => hydrate(provider)}>Reload saved options</button
    >{/if}
  <form onsubmit={save}>
    <label for="provider-options">Connection options</label>
    <textarea
      id="provider-options"
      bind:value={text}
      oninput={() => (dirty = true)}
      rows="16"
      spellcheck="false"
      disabled={!canManage || busy}></textarea>
    {#if canManage}<button class="button button-primary" disabled={busy}
        >Save options to draft</button
      >{/if}
  </form>
</details>

<style>
  .options {
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  summary {
    cursor: pointer;
    font-weight: 500;
  }
  p {
    color: var(--foreground-muted);
  }
  textarea {
    display: block;
    width: 100%;
    margin: 0.5rem 0 1rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-family: var(--font-mono);
    transition: border-color var(--motion);
  }
  textarea:hover {
    border-color: var(--border-strong);
  }
</style>
