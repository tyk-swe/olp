<script lang="ts">
  import { untrack } from 'svelte';
  import {
    useMutationIntents,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';

  import { createQuery } from '@tanstack/svelte-query';
  import { routeQueries } from '#lib/domain/routes/queries.js';
  import {
    activateProvider,
    disableProvider,
    restoreProviderAsDraft
  } from './providers';
  import { type ProviderSummary } from '#lib/domain/providers/api.js';
  import { ApiProblem, errorMessage } from '#lib/management/http.js';

  const intents = useMutationIntents();
  const rowAction = useAction();
  let {
    selected,
    onChanged
  }: { selected: ProviderSummary[]; onChanged: () => void | Promise<void> } =
    $props();
  let preview = $state<'disable' | 'activate' | 'restore' | null>(null);
  let busy = $state(false);
  let outcomes = $state<string[]>([]);
  let refreshPending = $state(false);
  type Action = NonNullable<typeof preview>;
  type Review = {
    action: Action;
    scope: string;
    targets: ProviderSummary[];
    confirmed: Set<string>;
    outcomes: Map<string, string>;
    ambiguous: Set<string>;
    refused: Set<string>;
  };
  let review: Review | undefined;
  let generation = 0;
  const selectionScope = $derived(
    JSON.stringify(selected.map((provider) => provider.id).sort())
  );
  function retire(principalChanged: boolean) {
    const stopped =
      busy &&
      review?.targets.some((target) => !review?.confirmed.has(target.id));
    generation += 1;
    review = undefined;
    intents.reset();
    busy = false;
    preview = null;
    refreshPending = false;
    if (principalChanged) outcomes = [];
    else if (stopped)
      outcomes = [...outcomes, 'Selection changed; stopped this bulk action.'];
  }
  const capturePrincipal = usePrincipalLifetime(() => retire(true));
  let previousSelection = untrack(() => selectionScope);
  $effect(() => {
    const next = selectionScope;
    if (next !== previousSelection) {
      previousSelection = next;
      untrack(() => retire(false));
    }
  });
  function beginReview(action: Action) {
    if (busy) return;
    if (
      !review ||
      review.action !== action ||
      review.scope !== selectionScope
    ) {
      generation += 1;
      intents.reset();
      refreshPending = false;
      review = {
        action,
        scope: selectionScope,
        targets: selected.map((provider) => ({ ...provider })),
        confirmed: new Set(),
        outcomes: new Map(),
        ambiguous: new Set(),
        refused: new Set()
      };
    } else {
      // Re-review can adopt a changed precondition only where the previous
      // request was definitely rejected and no earlier response was lost.
      review.targets = review.targets.map((target) => {
        if (!review?.refused.has(target.id) || review.ambiguous.has(target.id))
          return target;
        const current = selected.find((provider) => provider.id === target.id);
        if (!current || current.etag === target.etag) return target;
        review.refused.delete(target.id);
        return { ...current };
      });
    }
    preview = action;
    outcomes = [...review.outcomes.values()];
    void routes.refetch();
  }
  const routes = createQuery(() => ({
    ...routeQueries.all(),
    enabled: Boolean(preview)
  }));
  const affected = $derived(
    routes.data?.filter((route) =>
      route.latest_revision.targets.some((target) =>
        selected.some((provider) => provider.id === target.provider_id)
      )
    ) ?? []
  );
  async function refresh(isCurrent: () => boolean) {
    if (!isCurrent()) return;
    refreshPending = true;
    await onChanged();
    if (!isCurrent()) return;
    refreshPending = false;
    outcomes = [...(review?.outcomes.values() ?? [])];
    if (review?.targets.every((target) => review?.confirmed.has(target.id))) {
      review = undefined;
      preview = null;
    }
  }
  async function retryRefresh() {
    if (busy || !refreshPending || !review) return;
    const principalCurrent = capturePrincipal();
    const submittedReview = review;
    const submittedGeneration = generation;
    const isCurrent = () =>
      principalCurrent() &&
      generation === submittedGeneration &&
      review === submittedReview &&
      selectionScope === submittedReview.scope;
    busy = true;
    const result = await readResult(() => refresh(isCurrent));
    if (!result.ok && isCurrent())
      outcomes = [
        ...submittedReview.outcomes.values(),
        `Refreshing failed: ${errorMessage(result.error)}`
      ];
    if (generation === submittedGeneration && principalCurrent()) busy = false;
  }
  async function apply() {
    if (
      busy ||
      refreshPending ||
      !preview ||
      !review ||
      routes.isPending ||
      routes.isFetching ||
      routes.isError
    )
      return;
    const submittedReview = review;
    const submittedGeneration = generation;
    const principalCurrent = capturePrincipal();
    const isCurrent = () =>
      principalCurrent() &&
      generation === submittedGeneration &&
      review === submittedReview &&
      preview === submittedReview.action &&
      selectionScope === submittedReview.scope;
    busy = true;
    for (const provider of submittedReview.targets) {
      if (!isCurrent()) break;
      if (submittedReview.confirmed.has(provider.id)) continue;
      // The API enforces the reviewed selection's ETag. A fresh GET here
      // would refuse a committed request's replay before its key reaches
      // the idempotency boundary.
      const snap = { id: provider.id, etag: provider.etag };
      const send = (input: typeof snap, requestId: string): Promise<unknown> =>
        submittedReview.action === 'disable'
          ? disableProvider(input, requestId)
          : submittedReview.action === 'restore'
            ? restoreProviderAsDraft(input, requestId)
            : activateProvider(input, requestId);
      const key = `${submittedReview.action === 'disable' ? 'disableProvider' : submittedReview.action === 'restore' ? 'restoreProviderAsDraft' : 'activateProvider'}:${provider.id}`;
      await rowAction.run(
        () => intents.submit(key, snap, send),
        () => {
          submittedReview.confirmed.add(provider.id);
          submittedReview.outcomes.set(
            provider.id,
            `${provider.name}: ${submittedReview.action === 'activate' ? 'activated' : submittedReview.action === 'disable' ? 'disabled' : 'restored as draft'}.`
          );
          if (isCurrent()) refreshPending = true;
        },
        {
          key: provider.id,
          failure: (error) => {
            if (!isCurrent()) return '';
            const refused =
              error instanceof ApiProblem &&
              error.problem.status >= 400 &&
              error.problem.status < 500 &&
              ![408, 429].includes(error.problem.status) &&
              !error.problem.type?.endsWith('/idempotency_conflict');
            if (refused && !submittedReview.ambiguous.has(provider.id))
              submittedReview.refused.add(provider.id);
            else submittedReview.ambiguous.add(provider.id);
            submittedReview.outcomes.set(
              provider.id,
              `${provider.name}: ${errorMessage(error)}`
            );
            return errorMessage(error);
          }
        }
      );
      if (!isCurrent()) break;
      outcomes = [...submittedReview.outcomes.values()];
    }
    if (!isCurrent()) return;
    const result = await readResult(() => refresh(isCurrent));
    if (!result.ok && isCurrent())
      outcomes = [
        ...submittedReview.outcomes.values(),
        `Refreshing failed: ${errorMessage(result.error)}`
      ];
    if (generation === submittedGeneration && principalCurrent()) busy = false;
  }
</script>

{#if selected.length}
  <section class="card bulk-actions" aria-label="Selected connections">
    <strong>{selected.length} connections selected</strong>
    <div class="actions">
      {#each [{ action: 'activate', label: 'Activate selected' }, { action: 'disable', label: 'Disable selected' }, { action: 'restore', label: 'Restore as drafts' }] as item (item.action)}<button
          type="button"
          class="button button-secondary"
          disabled={busy}
          onclick={() => beginReview(item.action as Action)}
        >
          {item.label}
        </button>{/each}
    </div>
    {#if preview}<p>
        Review {selected.map((provider) => provider.name).join(', ')} before applying
        {preview}.
      </p>
      {#if routes.isPending || routes.isFetching}<p
          class="inline-status"
          role="status"
        >
          Checking affected routes…
        </p>{:else if routes.isError}<p class="inline-problem" role="alert">
          Route preview unavailable. <button onclick={() => routes.refetch()}>
            Retry
          </button>
        </p>{:else}<p>
          {affected.length
            ? `Published routes referencing these connections: ${affected.map((route) => route.slug).join(', ')}.`
            : 'No published routes reference these connections.'}
        </p>
        <p>
          {preview === 'disable'
            ? 'Remove referenced targets before disabling a connection. Connections with live media jobs remain protected.'
            : preview === 'activate'
              ? 'Each connection must have valid credentials and certified models. Changes apply to the listed routes.'
              : 'Connections return as drafts; review and validate them before activation.'}
        </p>
        <button
          class="button button-primary"
          disabled={busy || refreshPending}
          onclick={apply}
        >
          {busy ? 'Applying…' : `Apply ${preview} to reviewed connections`}
        </button>{/if}
    {/if}
  </section>
{/if}
{#if refreshPending && !busy}<button
    class="button button-secondary"
    type="button"
    onclick={retryRefresh}
  >
    Retry refresh
  </button>{/if}
{#if outcomes.length}<ul role="status">
    {#each outcomes as outcome (outcome)}<li>{outcome}</li>{/each}
  </ul>{/if}

<style>
  .bulk-actions {
    padding: 1.5rem;
    margin: 1rem 0;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    margin: 0.75rem 0;
  }
  p {
    color: var(--foreground-muted);
  }
</style>
