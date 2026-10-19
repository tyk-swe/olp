<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/lists/pagination';
  import { errorMessage } from '$lib/api/http';
  import { formatDate } from '$lib/format';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { useMutationIntent } from '$lib/forms/mutationIntent.svelte';
  import { listOrganizations } from '$lib/features/access/organizations/api';
  import { listProjects } from '$lib/features/access/projects/api';
  import { listProviderKinds } from '$lib/features/providers/api/models';
  import { listProviderVendors } from '$lib/features/providers/api/providers';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { organizationKeys } from '$lib/features/access/organizations/organizationKeys';
  import {
    createRateCard,
    createRateCardRevision,
    listRateCards,
    type CardPriceDraft,
    type RateCard
  } from '$lib/features/usage/api/rateCards';
  import { rateCardKeys } from '$lib/features/usage/discountKeys';
  import PriceFields from './PriceFields.svelte';
  import {
    emptyPriceFields,
    priceEntryFields,
    type PriceFieldValues
  } from './priceEntry';
  import { operationKinds } from './history/api';

  const createIntent = useMutationIntent();
  const publishIntent = useMutationIntent();
  const queryClient = useQueryClient();
  const access = useRole();
  const canEdit = $derived(access.can('pricing.update'));

  let ownerKind = $state<'installation' | 'organization' | 'project'>(
    'installation'
  );
  let ownerOrganization = $state('');
  let ownerProject = $state('');
  let creating = $state(false);
  let error = $state('');
  let notice = $state('');
  const pagination = $state(emptyCursorHistory());

  // The revision form edits one card at a time.
  let publishCardId = $state('');
  let markup = $state('');
  let effectiveAt = $state('');
  let publishing = $state(false);
  type PriceRow = PriceFieldValues & {
    providerKind: string;
    vendorId: string;
    model: string;
    operation: string;
  };
  const emptyRow = (): PriceRow => ({
    providerKind: providerKinds.data?.[0]?.kind ?? '',
    vendorId: '',
    model: '',
    operation: 'generation',
    ...emptyPriceFields()
  });
  let rows = $state<PriceRow[]>([]);

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
    if (!canEdit || creating) return;
    error = notice = '';
    const owner: { kind: typeof ownerKind; id?: string | null } = {
      kind: ownerKind
    };
    if (ownerKind === 'organization') owner.id = ownerOrganization || null;
    if (ownerKind === 'project') owner.id = ownerProject || null;
    if (ownerKind !== 'installation' && !owner.id) {
      error = `Choose the ${ownerKind} this card belongs to.`;
      return;
    }
    const submission = createIntent.prepare(owner);
    creating = true;
    try {
      await createRateCard(submission.input, submission.requestId);
      createIntent.reset();
      notice = 'Rate card created. Publish a revision to charge with it.';
      await queryClient.invalidateQueries({ queryKey: rateCardKeys.all });
    } catch (problem) {
      error = errorMessage(problem);
    } finally {
      creating = false;
    }
  }

  function rowDraft(row: PriceRow): CardPriceDraft | null {
    if (!row.providerKind || !row.model.trim()) return null;
    return {
      provider_kind: row.providerKind as CardPriceDraft['provider_kind'],
      vendor_id: row.vendorId || null,
      model: row.model.trim(),
      operation: row.operation as CardPriceDraft['operation'],
      ...priceEntryFields(row)
    };
  }

  async function publishRevision(event: SubmitEvent) {
    event.preventDefault();
    if (!canEdit || publishing || !publishCardId) return;
    error = notice = '';
    if (!markup.trim()) {
      error = 'A revision needs a markup.';
      return;
    }
    publishing = true;
    try {
      const prices = rows
        .map(rowDraft)
        .filter((price): price is CardPriceDraft => price != null);
      const submission = publishIntent.prepare({
        effective_at: effectiveAt
          ? new Date(effectiveAt).toISOString()
          : undefined,
        markup: markup.trim(),
        prices: prices.length ? prices : undefined
      });
      await createRateCardRevision(
        publishCardId,
        submission.input,
        submission.requestId
      );
      publishIntent.reset();
      notice = 'Revision published. New usage will charge under it.';
      publishCardId = '';
      rows = [];
      markup = '';
      await queryClient.invalidateQueries({ queryKey: rateCardKeys.all });
    } catch (problem) {
      error = errorMessage(problem);
    } finally {
      publishing = false;
    }
  }

  function startPublish(card: RateCard) {
    publishCardId = publishCardId === card.id ? '' : card.id;
    error = notice = '';
  }
</script>

<section class="stack" aria-labelledby="rate-cards-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Rate cards</p>
      <h2 id="rate-cards-title">Consumer charge terms</h2>
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
  {#if notice}<p class="inline-notice" role="status">{notice}</p>{/if}
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
      disabled={!canEdit || creating}
      >{creating ? 'Creating…' : 'Create card'}</button
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
    <div class="card empty-state" role="status">No rate cards.</div>
  {:else}
    <div class="table-shell" role="region" aria-label="Rate cards">
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
                  <form class="revision-form" onsubmit={publishRevision}>
                    <div class="form-grid">
                      <div class="form-field">
                        <label for={`markup-${card.id}`}>Markup</label><input
                          id={`markup-${card.id}`}
                          bind:value={markup}
                          inputmode="decimal"
                          placeholder="1.25"
                          required
                          disabled={!canEdit}
                        />
                      </div>
                      <div class="form-field">
                        <label for={`revision-effective-${card.id}`}
                          >Effective at</label
                        ><input
                          id={`revision-effective-${card.id}`}
                          bind:value={effectiveAt}
                          type="datetime-local"
                          aria-describedby="card-effective-help"
                          disabled={!canEdit}
                        /><small id="card-effective-help"
                          >Leave blank to apply immediately.</small
                        >
                      </div>
                    </div>
                    {#each rows as row, index (index)}
                      <fieldset class="extra-components card">
                        <legend>Explicit price {index + 1}</legend>
                        <div class="form-grid">
                          <div class="form-field">
                            <label for={`card-kind-${index}`}
                              >Provider kind</label
                            ><select
                              id={`card-kind-${index}`}
                              bind:value={row.providerKind}
                              disabled={!canEdit}
                              >{#each providerKinds.data ?? [] as option (option.kind)}<option
                                  value={option.kind}>{option.label}</option
                                >{/each}</select
                            >
                          </div>
                          <div class="form-field">
                            <label for={`card-vendor-${index}`}
                              >Vendor scope</label
                            ><select
                              id={`card-vendor-${index}`}
                              bind:value={row.vendorId}
                              disabled={!canEdit}
                              ><option value="">All vendors</option
                              >{#each (providerVendors.data ?? []).filter((vendor) => vendor.connector === row.providerKind) as vendor (vendor.id)}<option
                                  value={vendor.id}>{vendor.name}</option
                                >{/each}</select
                            >
                          </div>
                          <div class="form-field">
                            <label for={`card-model-${index}`}>Model</label
                            ><input
                              id={`card-model-${index}`}
                              bind:value={row.model}
                              required
                              disabled={!canEdit}
                            />
                          </div>
                          <div class="form-field">
                            <label for={`card-operation-${index}`}
                              >Operation</label
                            ><select
                              id={`card-operation-${index}`}
                              bind:value={row.operation}
                              disabled={!canEdit}
                              >{#each operationKinds as option (option)}<option
                                  value={option}>{option}</option
                                >{/each}</select
                            >
                          </div>
                        </div>
                        <PriceFields
                          idPrefix={`card-${index}`}
                          {canEdit}
                          bind:inputPrice={row.inputPrice}
                          bind:cachedInputPrice={row.cachedInputPrice}
                          bind:cacheWritePrice={row.cacheWritePrice}
                          bind:cacheWrite5mPrice={row.cacheWrite5mPrice}
                          bind:cacheWrite1hPrice={row.cacheWrite1hPrice}
                          bind:outputPrice={row.outputPrice}
                          bind:unitPrice={row.unitPrice}
                          bind:audioInputPrice={row.audioInputPrice}
                          bind:cachedAudioInputPrice={row.cachedAudioInputPrice}
                          bind:audioOutputPrice={row.audioOutputPrice}
                          bind:imageInputPrice={row.imageInputPrice}
                          bind:cachedImageInputPrice={row.cachedImageInputPrice}
                          bind:imageOutputPrice={row.imageOutputPrice}
                          bind:perRequestPrice={row.perRequestPrice}
                          bind:batchMultiplier={row.batchMultiplier}
                          bind:toolFees={row.toolFees}
                          bind:tiers={row.tiers}
                          bind:windowZone={row.windowZone}
                          bind:windowPeriods={row.windowPeriods}
                        />
                        <button
                          type="button"
                          class="text-button"
                          onclick={() => {
                            rows = rows.filter((_, i) => i !== index);
                          }}>Remove price</button
                        >
                      </fieldset>
                    {/each}
                    <button
                      type="button"
                      class="text-button"
                      onclick={() => {
                        rows = [...rows, emptyRow()];
                      }}>Add explicit price</button
                    >
                    <button
                      class="button button-primary"
                      type="submit"
                      disabled={!canEdit || publishing}
                      >{publishing ? 'Publishing…' : 'Publish revision'}</button
                    >
                  </form>
                </td>
              </tr>
            {/if}
          {/each}</tbody
        >
      </table>
    </div>
    <CursorPagination
      {...cursorPaginationProps(pagination, cards.data?.nextCursor)}
      label="Rate card pages"
    />
  {/if}
</section>
