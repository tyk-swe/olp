<script lang="ts">
  import { untrack } from 'svelte';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { stringifyNativeJSON } from '#lib/nativeJson.js';
  import { createQuery } from '@tanstack/svelte-query';
  import { collectCursorPages } from '#lib/management/pagination.js';
  import {
    ApiProblem,
    errorMessage,
    serializeIfMatch
  } from '#lib/management/http.js';
  import { providerKeys } from './providerKeys';
  import type { Provider } from './providers';
  import {
    listProviderModelPage,
    getProviderCapabilityOptions
  } from './models';
  import {
    desiredOperations,
    modelCapabilities,
    suggestedCapabilities,
    validateReviewedModel
  } from './bulkModelValidation.js';
  import { dialectSurface } from './profiles';
  import { listProviderProfiles } from './providerProfiles';
  import BulkModelInputs from './BulkModelInputs.svelte';
  import BulkModelStatus from './BulkModelStatus.svelte';
  const rowAction = useAction();
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
    const result = await readResult(() => refresh(isCurrent));
    if (!result.ok && isCurrent())
      failures = [{ id: null, message: errorMessage(result.error) }];
    if (isCurrent()) busy = false;
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
    const configuration = { ...snapshot.provider.configuration };
    const submittedOperation = operation;
    const vendor = configuration.options?.vendor_id ?? undefined;
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
    const state = {
      expectedETag: snapshot.provider.etag,
      mutationPending: false
    };
    const finish = () => {
      if (!isCurrent()) return;
      if (cancelled)
        status = `Stopped after ${completed} models. Review the results before activation.`;
      busy = false;
    };
    const matchesReviewedTarget = (current: Provider) => {
      if (
        providerTarget(current) === reviewedTarget &&
        serializeIfMatch(current.etag) === serializeIfMatch(state.expectedETag)
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
    const optionsResult = await readResult(() =>
      getProviderCapabilityOptions(
        configuration.kind,
        submittedController.signal
      )
    );
    if (!optionsResult.ok) {
      if (isCurrent())
        failures.push({ id: null, message: errorMessage(optionsResult.error) });
      finish();
      return;
    }
    if (!canContinue()) {
      finish();
      return;
    }
    const desired = desiredOperations(submittedOperation, vendor);
    const surfaceResult = await readResult(() => nativeSurface(configuration));
    if (!surfaceResult.ok) {
      if (isCurrent())
        failures.push({ id: null, message: errorMessage(surfaceResult.error) });
      finish();
      return;
    }
    if (!canContinue()) {
      finish();
      return;
    }
    const suggested = suggestedCapabilities(
      desired,
      surfaceResult.value,
      optionsResult.value
    );
    for (const model of targets) {
      if (!canContinue()) break;
      state.mutationPending = false;
      status = `Validating ${model.name} (${completed + 1}/${targets.length})…`;
      await rowAction.run(
        () =>
          validateReviewedModel({
            providerId,
            model,
            capabilities: modelCapabilities(
              model,
              submittedOperation,
              vendor,
              suggested
            ),
            reviewedMatch: matchesReviewedTarget,
            state,
            signal: submittedController.signal,
            canContinue,
            isCurrent,
            onCommitted: () => {
              refreshPending = true;
            }
          }),
        (outcome) => {
          if (outcome.status === 'checked' && !outcome.certified)
            failures.push({
              id: model.id,
              message: `${model.name}: one or more capabilities failed validation.`
            });
        },
        {
          key: model.id,
          failure: (e) => {
            if (!canContinue()) return '';
            failures.push({
              id: model.id,
              message: `${model.name}: ${errorMessage(e)}`
            });
            if (
              state.mutationPending &&
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
            }
            return errorMessage(e);
          }
        }
      );
      if (!canContinue()) break;
      completed += 1;
    }
    if (!isCurrent()) {
      finish();
      return;
    }
    status = `${cancelled ? 'Stopped after' : 'Checked'} ${completed} models. Review the results before activation.`;
    if (canContinue()) {
      const refreshed = await readResult(() => refresh(isCurrent));
      if (!refreshed.ok && isCurrent())
        failures.push({ id: null, message: errorMessage(refreshed.error) });
    }
    finish();
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
        onclick={() => models.refetch()}
      >
        Retry
      </button>
    </div>{/if}
  <BulkModelInputs
    items={models.data?.items ?? []}
    bind:operation
    bind:selected
    {busy}
    {canManage}
  />
  <BulkModelStatus
    {canManage}
    {busy}
    {refreshPending}
    selectedCount={selected.length}
    failedCount={failed.length}
    {status}
    failures={failures.map((failure) => failure.message)}
    onValidate={validateSelected}
    onRetryFailed={() => {
      selected = [...failed];
      retire();
      previousScope = scope;
      void validateSelected();
    }}
    onRetryRefresh={retryRefresh}
    onStop={() => {
      cancelled = true;
      controller?.abort();
    }}
  />
</section>

<style>
  .bulk {
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .description {
    color: var(--foreground-muted);
  }
  .description {
    margin-bottom: 1rem;
  }
</style>
