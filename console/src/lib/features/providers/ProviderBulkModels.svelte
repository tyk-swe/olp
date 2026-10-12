<script lang="ts">
  import { untrack } from 'svelte';
  import { usePrincipalLifetime } from '$lib/forms/mutationIntent.svelte';
  import { stringifyNativeJSON } from '$lib/json/nativeJson';
  import { createQuery } from '@tanstack/svelte-query';
  import { collectCursorPages } from '$lib/api/pagination';
  import { ApiProblem, errorMessage, serializeIfMatch } from '$lib/api/http';
  import { certificationPrerequisiteReady } from './providerEditor';
  import { providerKeys } from './providerKeys';
  import { getProvider, probeProvider, type Provider } from './api/providers';
  import {
    listProviderModelPage,
    setProviderModel,
    certifyProviderModel,
    getProviderCapabilityOptions
  } from './api/models';
  import { dialectSurface } from './profiles';
  import { listProviderProfiles } from './api/profiles';
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
  let refreshPending = $state(false);
  let controller: AbortController | undefined;
  let generation = 0;
  function providerTarget(value: Provider) {
    return stringifyNativeJSON([
      value.id,
      value.configuration,
      value.draft_credential_id
    ]);
  }
  function retire() {
    generation += 1;
    cancelled = true;
    controller?.abort();
    busy = false;
    refreshPending = false;
    status = '';
    failures = [];
  }
  const capturePrincipal = usePrincipalLifetime(() => {
    retire();
    selected = [];
  });
  const scope = $derived(
    JSON.stringify([
      providerTarget(provider),
      [...selected].sort(),
      operation,
      canManage
    ])
  );
  let previousScope = untrack(() => scope);
  $effect(() => {
    const next = scope;
    if (next !== previousScope) {
      previousScope = next;
      untrack(retire);
    }
  });
  let failures = $state<{ id: string | null; message: string }[]>([]);
  const failed = $derived(
    failures.flatMap((failure) => (failure.id ? [failure.id] : []))
  );
  const models = createQuery(() => {
    const id = provider.id;
    const etag = provider.etag;
    return {
      queryKey: providerKeys.bulkModels(id, etag),
      queryFn: async ({ signal }) => {
        let snapshot: Provider | undefined;
        const items = await collectCursorPages(async (cursor) => {
          const page = await listProviderModelPage(id, cursor, signal);
          if (
            snapshot &&
            serializeIfMatch(snapshot.etag) !==
              serializeIfMatch(page.provider.etag)
          ) {
            throw new Error(
              'The connection changed while models were loaded. Refresh before reviewing them.'
            );
          }
          snapshot ??= page.provider;
          return page;
        });
        if (!snapshot)
          throw new Error('The model inventory has no connection version.');
        return { items, provider: snapshot };
      }
    };
  });
  /** The client surface that speaks the provider's dialect natively. */
  async function nativeSurface(configuration: Provider['configuration']) {
    const { kind, profile_id, profile_revision } = configuration;
    if (kind === 'anthropic') return 'anthropic';
    if (kind === 'gemini' || kind === 'vertex_ai') return 'gemini';
    if (kind !== 'plugin') return 'openai';
    // A plugin profile names its dialect.
    const profile = (await listProviderProfiles()).find(
      (candidate) =>
        candidate.id === profile_id && candidate.revision === profile_revision
    );
    return dialectSurface(profile?.dialect);
  }
  async function refresh(isCurrent: () => boolean) {
    if (!isCurrent()) return;
    await onChanged();
    if (!isCurrent()) return;
    await models.refetch({ throwOnError: true });
    if (isCurrent()) {
      refreshPending = false;
      failures = failures.filter((failure) => failure.id !== null);
    }
  }
  async function retryRefresh() {
    if (busy || !refreshPending || !canManage) return;
    const principalCurrent = capturePrincipal();
    const submittedScope = scope;
    const submittedGeneration = generation;
    const isCurrent = () =>
      principalCurrent() &&
      generation === submittedGeneration &&
      scope === submittedScope;
    busy = true;
    try {
      await refresh(isCurrent);
    } catch (error) {
      if (isCurrent()) failures = [{ id: null, message: errorMessage(error) }];
    } finally {
      if (isCurrent()) busy = false;
    }
  }
  async function validateSelected() {
    if (busy || refreshPending || !canManage || !selected.length) return;
    const principalCurrent = capturePrincipal();
    const submittedScope = scope;
    const submittedGeneration = ++generation;
    const providerId = provider.id;
    const snapshot = models.data;
    if (
      !snapshot ||
      providerTarget(snapshot.provider) !== providerTarget(provider)
    ) {
      refreshPending = true;
      failures = [
        {
          id: null,
          message:
            'The connection changed. Refresh before validating these models.'
        }
      ];
      return;
    }
    const reviewedTarget = providerTarget(snapshot.provider);
    let expectedETag = snapshot.provider.etag;
    const configuration = { ...snapshot.provider.configuration };
    const submittedOperation = operation;
    const vendor = configuration.options?.vendor_id;
    const targets = selected.flatMap((id) => {
      const model = snapshot.items.find((candidate) => candidate.id === id);
      return model
        ? [
            {
              id,
              name: model.display_name,
              capabilities: model.capabilities.map(
                ({ operation, surface, mode }) => ({ operation, surface, mode })
              )
            }
          ]
        : [];
    });
    const isCurrent = () =>
      principalCurrent() &&
      generation === submittedGeneration &&
      scope === submittedScope;
    const canContinue = () => isCurrent() && !cancelled;
    busy = true;
    const submittedController = new AbortController();
    controller = submittedController;
    cancelled = false;
    failures = [];
    let completed = 0;
    const matchesReviewedTarget = (current: Provider) => {
      if (
        providerTarget(current) === reviewedTarget &&
        serializeIfMatch(current.etag) === serializeIfMatch(expectedETag)
      )
        return true;
      cancelled = true;
      submittedController.abort();
      refreshPending = true;
      failures.push({
        id: null,
        message:
          'The connection changed. Refresh before validating these models.'
      });
      return false;
    };
    try {
      const options = await getProviderCapabilityOptions(
        configuration.kind,
        submittedController.signal
      );
      if (!canContinue()) return;
      const desired =
        submittedOperation === 'configured'
          ? vendor === 'voyage'
            ? ['embeddings']
            : ['generation']
          : [submittedOperation];
      const surface = await nativeSurface(configuration);
      if (!canContinue()) return;
      const candidates = options.capabilities.filter(
        (tuple) =>
          desired.includes(tuple.operation) &&
          (tuple.mode === 'unary' || tuple.mode === 'streaming')
      );
      // Prefer the native surface, but some operations (embeddings on
      // Gemini, for example) are only certifiable on another surface.
      const suggested = desired.flatMap((op) => {
        const forOp = candidates.filter((tuple) => tuple.operation === op);
        const native = forOp.filter((tuple) => tuple.surface === surface);
        return native.length ? native : forOp;
      });
      for (const model of targets) {
        if (!canContinue()) break;
        const id = model.id;
        status = `Validating ${model.name} (${completed + 1}/${targets.length})…`;
        let mutationPending = false;
        try {
          let current = await getProvider(
            providerId,
            submittedController.signal
          );
          if (!canContinue()) break;
          if (!matchesReviewedTarget(current)) break;
          if (!certificationPrerequisiteReady(current)) {
            mutationPending = true;
            const probe = await probeProvider(current);
            mutationPending = false;
            expectedETag = probe.etag;
            if (!canContinue()) break;
            if (!probe.result.succeeded) throw new Error(probe.result.detail);
            current = await getProvider(providerId, submittedController.signal);
            if (!canContinue()) break;
            if (!matchesReviewedTarget(current)) break;
          }
          mutationPending = true;
          current = await setProviderModel(
            current,
            id,
            true,
            submittedOperation === 'configured' &&
              vendor !== 'voyage' &&
              model.capabilities.length
              ? // Responses carry source and certification fields the
                // request schema refuses.
                model.capabilities
              : suggested
          );
          mutationPending = false;
          expectedETag = current.etag;
          if (isCurrent()) refreshPending = true;
          if (!canContinue()) break;
          mutationPending = true;
          const certification = await certifyProviderModel(
            current,
            id,
            submittedController.signal
          );
          mutationPending = false;
          expectedETag = certification.etag;
          if (!canContinue()) break;
          const outcome = certification.result;
          if (
            !outcome.results.length ||
            !outcome.results.every((item) => item.succeeded)
          ) {
            failures.push({
              id,
              message: `${model.name}: one or more capabilities failed validation.`
            });
          }
        } catch (e) {
          if (!canContinue()) break;
          failures.push({
            id,
            message: `${model.name}: ${errorMessage(e)}`
          });
          if (
            mutationPending &&
            !(
              e instanceof ApiProblem &&
              e.problem.status >= 400 &&
              e.problem.status < 500 &&
              e.problem.status !== 408 &&
              e.problem.status !== 429 &&
              !e.problem.type?.endsWith('/idempotency_conflict')
            )
          ) {
            cancelled = true;
            submittedController.abort();
            refreshPending = true;
            break;
          }
        }
        completed += 1;
      }
      if (!isCurrent()) return;
      status = `${cancelled ? 'Stopped after' : 'Checked'} ${completed} models. Review the results before activation.`;
      if (canContinue()) await refresh(isCurrent);
    } catch (e) {
      if (!isCurrent()) return;
      failures.push({ id: null, message: errorMessage(e) });
    } finally {
      if (isCurrent()) {
        if (cancelled)
          status = `Stopped after ${completed} models. Review the results before activation.`;
        busy = false;
      }
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
    {#each models.data?.items ?? [] as model (model.id)}<label
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
        disabled={busy || refreshPending || !selected.length}
        onclick={validateSelected}
        >Validate {selected.length} selected models</button
      >{/if}
    {#if failed.length && !busy && !refreshPending}<button
        class="button button-secondary"
        type="button"
        onclick={() => {
          selected = [...failed];
          retire();
          previousScope = scope;
          void validateSelected();
        }}>Retry {failed.length} failed models</button
      >{/if}
    {#if refreshPending && !busy}<button
        class="button button-secondary"
        type="button"
        onclick={retryRefresh}>Retry refresh</button
      >{/if}
    {#if busy}<button
        class="button button-secondary"
        type="button"
        onclick={() => {
          cancelled = true;
          controller?.abort();
        }}>Stop validation</button
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
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
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
