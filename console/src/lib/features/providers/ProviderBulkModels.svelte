<script lang="ts">
  import { onDestroy } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { collectCursorPages } from '$lib/api/pagination';
  import { errorMessage } from '$lib/api/http';
  import { certificationPrerequisiteReady } from './providerEditor';
  import { getProvider, probeProvider, type Provider } from './api';
  import {
    listProviderModelPage,
    setProviderModel,
    certifyProviderModel,
    getProviderCapabilityOptions
  } from './models';
  let {
    provider,
    canManage,
    onChanged
  }: {
    provider: Provider;
    canManage: boolean;
    onChanged: () => void | Promise<void>;
  } = $props();
  let selected = $state<string[]>([]);
  let operation = $state('configured');
  let busy = $state(false);
  let cancelled = $state(false);
  let status = $state('');
  onDestroy(() => {
    cancelled = true;
  });
  let failures = $state<{ id: string | null; message: string }[]>([]);
  const failed = $derived(
    failures.flatMap((failure) => (failure.id ? [failure.id] : []))
  );
  const models = createQuery(() => ({
    queryKey: ['bulk-provider-models', provider.id, provider.etag],
    queryFn: () =>
      collectCursorPages((cursor) => listProviderModelPage(provider.id, cursor))
  }));
  async function validateSelected() {
    busy = true;
    cancelled = false;
    failures = [];
    let completed = 0;
    try {
      const options = await getProviderCapabilityOptions(
        provider.configuration.kind
      );
      const vendor = provider.configuration.options?.vendor_id;
      const desired =
        operation === 'configured'
          ? vendor === 'voyage'
            ? ['embeddings']
            : ['generation']
          : [operation];
      const suggested = options.capabilities.filter(
        (tuple) =>
          desired.includes(tuple.operation) &&
          (tuple.mode === 'unary' || tuple.mode === 'streaming') &&
          tuple.surface ===
            (provider.configuration.kind === 'anthropic'
              ? 'anthropic'
              : ['gemini', 'vertex_ai'].includes(provider.configuration.kind)
                ? 'gemini'
                : 'openai')
      );
      for (const id of selected) {
        if (cancelled) break;
        const model = models.data?.find((m) => m.id === id);
        if (!model) continue;
        status = `Validating ${model.display_name} (${completed + 1}/${selected.length})…`;
        try {
          let current = await getProvider(provider.id);
          if (!certificationPrerequisiteReady(current)) {
            const probe = await probeProvider(current);
            if (!probe.succeeded) throw new Error(probe.detail);
            current = await getProvider(provider.id);
          }
          current = await setProviderModel(
            current,
            id,
            true,
            operation === 'configured' &&
              vendor !== 'voyage' &&
              model.capabilities.length
              ? model.capabilities
              : suggested
          );
          const outcome = await certifyProviderModel(current, id);
          if (
            !outcome.results.length ||
            !outcome.results.every((item) => item.succeeded)
          ) {
            failures.push({
              id,
              message: `${model.display_name}: one or more capabilities failed validation.`
            });
          }
        } catch (e) {
          failures.push({
            id,
            message: `${model.display_name}: ${errorMessage(e)}`
          });
        }
        completed += 1;
      }
      status = `${cancelled ? 'Stopped after' : 'Checked'} ${completed} models. Review the results before activation.`;
      await onChanged();
      await models.refetch();
    } catch (e) {
      failures.push({ id: null, message: errorMessage(e) });
    } finally {
      busy = false;
    }
  }
</script>

<section class="card bulk" aria-labelledby="bulk-heading">
  <p class="eyebrow">Bulk validation</p>
  <h2 id="bulk-heading">Validate models in bulk</h2>
  <p class="description">
    Select the models to enable. Validation uses bounded upstream requests and
    records a result for each capability.
  </p>
  {#if models.isError}<div class="inline-problem" role="alert">
      Models could not be loaded. <button
        class="button button-secondary"
        type="button"
        onclick={() => models.refetch()}>Retry</button
      >
    </div>{/if}
  <div class="form-field">
    <label for="bulk-operation">Capabilities to validate</label><select
      id="bulk-operation"
      bind:value={operation}
      disabled={busy || !canManage}
      ><option value="configured">Keep configured capabilities</option><option
        value="generation">Generation (unary and streaming)</option
      ><option value="embeddings">Embeddings</option><option value="token_count"
        >Token count</option
      ></select
    >
  </div>
  <div class="model-list">
    {#each models.data ?? [] as model (model.id)}<label
        ><input
          type="checkbox"
          value={model.id}
          bind:group={selected}
          disabled={!canManage || busy}
        /><span>{model.display_name}<small>{model.upstream_model}</small></span
        ></label
      >{/each}
  </div>
  <div class="form-actions">
    {#if canManage}<button
        class="button button-primary"
        type="button"
        disabled={busy || !selected.length}
        onclick={validateSelected}
        >Validate {selected.length} selected models</button
      >{/if}
    {#if failed.length && !busy}<button
        class="button button-secondary"
        type="button"
        onclick={() => {
          selected = [...failed];
          void validateSelected();
        }}>Retry {failed.length} failed models</button
      >{/if}
    {#if busy}<button
        class="button button-secondary"
        type="button"
        onclick={() => {
          cancelled = true;
        }}>Stop after current model</button
      >{/if}
  </div>
  {#if status}<p class="status" role="status">{status}</p>{/if}
  {#if failures.length}<ul class="inline-problem" role="alert">
      {#each failures as failure (failure.message)}<li>
          {failure.message}
        </li>{/each}
    </ul>{/if}
</section>

<style>
  .bulk {
    padding: clamp(1.15rem, 3vw, 1.75rem);
  }
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .description,
  .status,
  small {
    color: var(--foreground-muted);
  }
  .description {
    margin-bottom: 1rem;
  }
  .status {
    margin-top: 1rem;
  }
  .form-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
  }
  .model-list {
    max-height: 20rem;
    overflow: auto;
    margin: 1rem 0;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(15rem, 1fr));
    gap: 0.5rem;
  }
  .model-list label {
    display: flex;
    align-items: start;
    gap: 0.5rem;
    padding: 0.6rem;
  }
  small {
    display: block;
    overflow-wrap: anywhere;
  }
</style>
