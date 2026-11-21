<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import { errorMessage } from '#lib/management/http.js';
  import { formatDate } from '#lib/format.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import {
    listOrganizations,
    listProjects,
    projectKeys,
    organizationKeys
  } from '#lib/features/access/index.js';
  import {
    listProviderKinds,
    listProviderVendors,
    providerKeys
  } from '#lib/features/providers/index.js';
  import {
    createRateCard,
    createRateCardRevision,
    listRateCards,
    type RateCard
  } from '#lib/features/usage/rateCards.js';
  import { rateCardKeys } from '#lib/features/usage/discountKeys.js';
  import RateCardRevisionForm from './RateCardRevisionForm.svelte';
  import { cardPriceDraft, type PriceEntryRow } from './priceEntry';

  const create = useMutation();
  const publish = useMutation();
  const queryClient = useQueryClient();
  const access = useRole();
  const canEdit = $derived(access.can('pricing.update'));

  let ownerKind = $state<'installation' | 'organization' | 'project'>(
    'installation'
  );
  let ownerOrganization = $state('');
  let ownerProject = $state('');
  let notice = $state('');
  const error = $derived(create.error || publish.error);
  const pagination = $state(emptyCursorHistory());

  // The revision form edits one card at a time.
  let publishCardId = $state('');
  let markup = $state('');
  let effectiveAt = $state('');
  let rows = $state<PriceEntryRow[]>([]);

  const organizations = createQuery(() => ({
    queryKey: organizationKeys.all,
    queryFn: ({ signal }) => listOrganizations(signal)
  }));
  const projects = createQuery(() => ({
    queryKey: projectKeys.all,
    queryFn: ({ signal }) => listProjects(signal)
  }));
  const providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  const providerVendors = createQuery(() => ({
    queryKey: providerKeys.vendors(),
    queryFn: () => listProviderVendors()
  }));

  const cards = createQuery(() => {
    const cursor = pagination.cursor;
    return {
      queryKey: rateCardKeys.page(cursor),
      queryFn: () => listRateCards(cursor)
    };
  });

  function ownerText(card: RateCard): string {
    const owner = card.owner;
    if (owner.kind === 'project')
      return `Project ${owner.project ?? owner.project_id}`;
    if (owner.kind === 'organization')
      return `Organization ${owner.organization ?? owner.organization_id}`;
    return 'Installation';
  }

  async function addCard(event: SubmitEvent) {
    event.preventDefault();
    if (!canEdit || create.busy) return;
    notice = '';
    const owner: { kind: typeof ownerKind; id?: string | null } = {
      kind: ownerKind
    };
    if (ownerKind === 'organization') owner.id = ownerOrganization || null;
    if (ownerKind === 'project') owner.id = ownerProject || null;
    if (ownerKind !== 'installation' && !owner.id) {
      create.error = `Choose the ${ownerKind} this card belongs to.`;
      return;
    }
    await create.submit(
      owner,
      (input, requestId) => createRateCard(input, requestId),
      async () => {
        notice = 'Rate card created. Publish a revision to charge with it.';
        await queryClient.invalidateQueries({ queryKey: rateCardKeys.all });
      },
      { failure: (problem) => errorMessage(problem) }
    );
  }

  async function publishRevision(event: SubmitEvent) {
    event.preventDefault();
    if (!canEdit || publish.busy || !publishCardId) return;
    notice = '';
    if (!markup.trim()) {
      publish.error = 'A revision needs a markup.';
      return;
    }
    const cardId = publishCardId;
    const input = {
      effective_at: effectiveAt
        ? new Date(effectiveAt).toISOString()
        : undefined,
      markup: markup.trim(),
      prices: rows
        .map(cardPriceDraft)
        .filter((price): price is NonNullable<typeof price> => price != null)
    };
    await publish.submit(
      {
        ...input,
        prices: input.prices.length ? input.prices : undefined
      },
      (submission, requestId) =>
        createRateCardRevision(cardId, submission, requestId),
      async () => {
        notice = 'Revision published. New usage will charge under it.';
        publishCardId = '';
        rows = [];
        markup = '';
        await queryClient.invalidateQueries({ queryKey: rateCardKeys.all });
      },
      { failure: (problem) => errorMessage(problem) }
    );
  }

  function startPublish(card: RateCard) {
    publishCardId = publishCardId === card.id ? '' : card.id;
    create.reset();
    publish.reset();
    notice = '';
  }
</script>

<section class="stack" aria-labelledby="rate-cards-title">
  <div class="section-heading">
    <div>
      <h2 class="sr-only" id="rate-cards-title">Consumer charge terms</h2>
      <p class="section-description">
        Each card charges its owner's consumers a markup on provider cost, or
        explicit rates; a project card wins over its organization's, then the
        installation's.
      </p>
    </div>
  </div>
  {#if !canEdit}<ReadOnlyNote
      >Your role can view rate cards but not publish them.</ReadOnlyNote
    >{/if}
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if notice}<Callout tone="success">{notice}</Callout>{/if}
  <form class="card card-form" onsubmit={addCard}>
    <div class="form-grid">
      <div class="form-field">
        <label for="card-owner-kind">Owner kind</label><select
          id="card-owner-kind"
          bind:value={ownerKind}
          disabled={!canEdit}
          ><option value="installation">Installation</option><option
            value="organization">Organization</option
          ><option value="project">Project</option></select
        >
      </div>
      {#if ownerKind === 'organization'}
        <div class="form-field">
          <label for="card-organization">Organization</label><select
            id="card-organization"
            bind:value={ownerOrganization}
            disabled={!canEdit}
            ><option value="">Choose an organization</option
            >{#each organizations.data ?? [] as org (org.id)}<option
                value={org.id}>{org.name}</option
              >{/each}</select
          >
        </div>
      {:else if ownerKind === 'project'}
        <div class="form-field">
          <label for="card-project">Project</label><select
            id="card-project"
            bind:value={ownerProject}
            disabled={!canEdit}
            ><option value="">Choose a project</option
            >{#each projects.data ?? [] as project (project.id)}<option
                value={project.id}>{project.name}</option
              >{/each}</select
          >
        </div>
      {/if}
    </div>
    <button
      class="button button-primary"
      type="submit"
      disabled={!canEdit || create.busy}
      >{create.busy ? 'Creating…' : 'Create card'}</button
    >
  </form>
  {#if cards.isPending}<div class="loading-state" role="status">
      Loading rate cards…
    </div>
  {:else if cards.isError}<div class="inline-problem" role="alert">
      {errorMessage(cards.error)}
      <button class="text-button" onclick={() => cards.refetch()}
        >Try again</button
      >
    </div>
  {:else if (cards.data?.items ?? []).length === 0}
    <EmptyState title="No rate cards yet"
      >A rate card sets what consumers are charged, separate from what the
      provider costs you.</EmptyState
    >
  {:else}
    <ScrollTable label="Rate cards">
      <table class="data-table">
        <caption class="sr-only">Rate cards by owner</caption>
        <thead
          ><tr
            ><th scope="col">Owner</th><th scope="col">Revisions</th><th
              scope="col">Created</th
            ><th scope="col">Actions</th></tr
          ></thead
        >
        <tbody
          >{#each cards.data?.items ?? [] as card (card.id)}<tr>
              <td>{ownerText(card)}</td>
              <td
                >{#each card.revisions ?? [] as revision (revision.id)}<span
                    class="mono"
                    >r{revision.revision} ×{revision.markup}
                    {formatDate(revision.effective_at)}</span
                  ><br
                  />{/each}{#if (card.revisions ?? []).length === 0}—{/if}</td
              >
              <td>{formatDate(card.created_at)}</td>
              <td>
                <button
                  class="text-button"
                  type="button"
                  disabled={!canEdit}
                  onclick={() => startPublish(card)}
                  >{publishCardId === card.id
                    ? 'Close'
                    : 'Publish revision'}</button
                >
              </td>
            </tr>
            {#if publishCardId === card.id}
              <tr>
                <td colspan="4">
                  <RateCardRevisionForm
                    cardId={card.id}
                    bind:rows
                    bind:markup
                    bind:effectiveAt
                    {canEdit}
                    busy={publish.busy}
                    kinds={providerKinds.data ?? []}
                    vendors={providerVendors.data ?? []}
                    onSubmit={publishRevision}
                  />
                </td>
              </tr>
            {/if}
          {/each}</tbody
        >
      </table>
    </ScrollTable>
    <CursorPagination
      {...cursorPaginationProps(pagination, cards.data?.nextCursor)}
      label="Rate card pages"
    />
  {/if}
</section>
