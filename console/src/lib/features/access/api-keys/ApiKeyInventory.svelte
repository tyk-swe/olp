<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import { personOptions } from '#lib/features/identity/index.js';
  import { confirmAction } from '#lib/components/confirm.js';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import ApiKeyTable from './ApiKeyTable.svelte';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  const services = useServiceCapabilities();
  import { apiKeyKeys } from '#lib/features/access/api-keys/apiKeyKeys.js';
  import { overviewKeys } from '#lib/features/overview/index.js';

  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import {
    cursorPaginationProps,
    resetCursor
  } from '#lib/listing/pagination.js';

  import {
    getApiKeyPauseRollout,
    listApiKeyPage,
    pauseApiKey,
    resumeApiKey,
    revokeApiKey,
    rotateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '#lib/features/access/api-keys/api.js';
  import SecretDialog from '#lib/components/SecretDialog.svelte';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import type { ApiKeyListState } from '#lib/features/access/api-keys/apiKeyListState.js';

  const write = useAction();
  const rotateIntent = useMutationIntent();
  const revokeIntent = useMutationIntent();
  const pauseIntent = useMutationIntent();
  const resumeIntent = useMutationIntent();

  let {
    listState = $bindable(),
    submitError,
    canManage,
    onSecret
  }: {
    listState: ApiKeyListState;
    submitError: string;
    canManage: boolean;
    onSecret: (secret: ApiKeySecret, preferredRoute?: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  let rotating = $state<ApiKey | null>(null);
  let overlapSeconds = $state('0');
  let rotationError = $state('');
  let rotationAttempted = $state(false);
  let pauseSavedFor = $state('');
  usePrincipalLifetime(() => {
    rotating = null;
    overlapSeconds = '0';
    rotationAttempted = false;
    rotationError = '';
    pauseSavedFor = '';
  });
  const pauseRollout = createQuery(() => ({
    queryKey: apiKeyKeys.pauseRollout(pauseSavedFor),
    queryFn: ({ signal }) => getApiKeyPauseRollout(pauseSavedFor, signal),
    enabled: Boolean(pauseSavedFor),
    refetchInterval: (query) => (query.state.data?.enforced ? false : 5000)
  }));
  const createdBy = $derived(
    page.url.searchParams.get('created_by')?.trim().toLowerCase() || undefined
  );
  let issuer = $state('');
  const cursor = $derived(
    listState.createdBy === createdBy ? listState.cursor : undefined
  );
  $effect(() => {
    issuer = createdBy ?? '';
    if (listState.createdBy !== createdBy) {
      resetCursor(listState);
      listState.createdBy = createdBy;
    }
  });
  const keys = createQuery(() => ({
    queryKey: apiKeyKeys.page(cursor, createdBy),
    queryFn: ({ signal }) => listApiKeyPage(cursor, signal, createdBy)
  }));

  async function filterIssuer(value: string) {
    const url = new URL(page.url.href);
    const id = value.trim().toLowerCase();
    if (id) url.searchParams.set('created_by', id);
    else url.searchParams.delete('created_by');
    const target = url.search
      ? resolve(`keys?${url.searchParams}${url.hash}`)
      : url.hash
        ? resolve(`keys#${url.hash.slice(1)}`)
        : resolve('keys');

    await goto(target, { reset: false });
  }

  function openRotation(key: ApiKey) {
    rotating = key;
    overlapSeconds = '0';
    rotationAttempted = false;
    rotateIntent.reset();
    rotationError = '';
  }

  async function rotate(key: ApiKey) {
    if (!/^\d+$/.test(overlapSeconds) || Number(overlapSeconds) > 86400) {
      rotationError =
        'Overlap must be a whole number of seconds from 0 to 86400.';

      return;
    }
    rotationAttempted = true;
    await write.run(
      () =>
        rotateIntent.submit(
          { key, seconds: Number(overlapSeconds) },
          (input, requestId) =>
            rotateApiKey(input.key, input.seconds, requestId)
        ),
      async (replacement) => {
        rotating = null;
        onSecret(replacement, key.allowed_routes[0]);
        await queryClient.invalidateQueries({ queryKey: apiKeyKeys.root });
      },
      {
        key: `rotate-${key.id}`,
        failure: (cause) => {
          const message = errorMessage(cause);
          rotationError = message;
          return message;
        }
      }
    );
  }

  async function pause(key: ApiKey) {
    if (
      !(await confirmAction({
        title: `Pause ${key.name}?`,
        body: 'New requests using this key are refused once serving replicas pick up the change. Already admitted work is not canceled. Rotate or revoke a key you suspect is compromised instead.',
        confirmLabel: 'Pause key'
      }))
    )
      return;
    await write.run(
      () =>
        pauseIntent.submit({ id: key.id, etag: key.etag }, (input, requestId) =>
          pauseApiKey(input, requestId)
        ),
      async () => {
        pauseSavedFor = key.id;
        await queryClient.invalidateQueries({ queryKey: apiKeyKeys.root });
      },
      { key: `pause-${key.id}` }
    );
  }

  async function resume(key: ApiKey) {
    await write.run(
      () =>
        resumeIntent.submit(
          { id: key.id, etag: key.etag },
          (input, requestId) => resumeApiKey(input, requestId)
        ),
      async () => {
        if (pauseSavedFor === key.id) pauseSavedFor = '';
        await queryClient.invalidateQueries({ queryKey: apiKeyKeys.root });
      },
      { key: `resume-${key.id}` }
    );
  }

  async function revoke(key: ApiKey) {
    if (
      !(await confirmAction({
        title: `Revoke ${key.name}?`,
        body: 'Requests using this key start failing right away. You can’t undo this.',
        confirmLabel: 'Revoke key',
        danger: true
      }))
    )
      return;
    await write.run(
      () =>
        revokeIntent.submit(
          { id: key.id, etag: key.etag },
          (input, requestId) => revokeApiKey(input, requestId)
        ),
      async () => {
        await queryClient.invalidateQueries({ queryKey: apiKeyKeys.root });
        await queryClient.invalidateQueries({ queryKey: overviewKeys.root });
      },
      { key: `revoke-${key.id}` }
    );
  }
</script>

<PageHeader
  title="API keys"
  description="Manage client access with scoped API keys, route permissions, and usage limits."
>
  {#snippet actions()}
    {#if canManage}<a class="button button-primary" href={resolve('keys/new')}>
        New API key <NavIcon name="arrow" />
      </a>
    {/if}
  {/snippet}
</PageHeader>
{#if !services.limitsEnforced}<p class="muted" role="status">
    Limit and budget policies are saved but not enforced yet.
  </p>{/if}
{#if !canManage}
  <ReadOnlyNote>
    Your role can view API keys but not create, edit, rotate, or revoke them.
  </ReadOnlyNote>
{/if}
{#if submitError || write.error}<div class="key-problem">
    <Callout tone="danger">{submitError || write.error}</Callout>
  </div>{/if}
{#if pauseSavedFor}<p class="muted" role="status">
    {pauseRollout.data?.paused && pauseRollout.data.enforced
      ? 'Pause enforced across serving replicas'
      : 'Pause saved. Enforcement across replicas is not yet confirmed.'}
  </p>{/if}

<div class="toolbar issuer-filter">
  <label for="key-issuer">
    Created by <EntityPicker
      id="key-issuer"
      source={personOptions}
      bind:value={issuer}
      emptyLabel="Anyone"
      onchange={filterIssuer}
    />
  </label>
</div>

<QueryView
  query={keys}
  loading="Loading API keys…"
  failure={errorMessage(keys.error)}
  emptyWhen={!keys.data?.items.length && listState.history.length === 0}
>
  {#snippet empty()}
    <EmptyState
      title={createdBy ? 'No API keys created by this person' : 'No API keys'}
    >
      {createdBy
        ? 'Pick someone else or show every key.'
        : 'Create a scoped key for your clients.'}
      {#snippet action()}
        {#if createdBy}<button
            class="button button-secondary"
            type="button"
            onclick={() => filterIssuer('')}
          >
            Show all keys
          </button>{/if}
        {#if canManage && !createdBy}<a
            class="button button-primary"
            href={resolve('keys/new')}
          >
            Create first key
          </a>
        {/if}
      {/snippet}
    </EmptyState>
  {/snippet}
  <ApiKeyTable
    items={keys.data?.items ?? []}
    {canManage}
    limitsEnforced={services.limitsEnforced}
    gatewayAvailable={services.gatewayAvailable}
    busy={write.busy}
    busyKey={write.key}
    onRotate={openRotation}
    onPause={pause}
    onResume={resume}
    onRevoke={revoke}
  />
  <CursorPagination
    {...cursorPaginationProps(listState, keys.data?.nextCursor)}
    label="API key pages"
  />
</QueryView>

{#if rotating}
  <SecretDialog
    eyebrow="Explicit rotation"
    title={`Rotate ${rotating.name}`}
    description="Generate a new secret and choose how long the previous secret remains valid. All versions share the key's current permissions, limits and budgets."
    onClose={() => {
      rotating = null;
    }}
  >
    {#snippet children(close)}
      <form
        onsubmit={(event) => {
          event.preventDefault();
          if (rotating) void rotate(rotating);
        }}
      >
        <div class="form-field full">
          <label for="rotation-overlap">
            Previous-secret overlap (seconds)
          </label><input
            id="rotation-overlap"
            data-autofocus
            inputmode="numeric"
            bind:value={overlapSeconds}
            disabled={write.busy || rotationAttempted}
            aria-describedby="rotation-overlap-help"
          /><small id="rotation-overlap-help">
            0 ends all previous overlaps when gateways refresh. A positive value
            allows up to 24 hours, bounded by key expiry; existing overlap
            deadlines stay unchanged.
          </small>
        </div>
        {#if rotationError}<div class="inline-problem" role="alert">
            {rotationError} Retry uses the same request and overlap to recover its
            result. Close this dialog to start a new rotation.
          </div>{/if}
        <div class="form-actions">
          <button
            class="button button-primary"
            type="submit"
            disabled={write.busy}
          >
            Rotate key
          </button><button
            class="button button-secondary"
            type="button"
            onclick={close}
            disabled={write.busy}
          >
            Cancel
          </button>
        </div>
      </form>
    {/snippet}
  </SecretDialog>
{/if}

<style>
  .key-problem {
    margin-top: 1rem;
  }
  .issuer-filter {
    margin-top: 1.5rem;
  }
  .issuer-filter label {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .issuer-filter :global(:is(select, input)) {
    min-width: min(18rem, 60vw);
  }
</style>
