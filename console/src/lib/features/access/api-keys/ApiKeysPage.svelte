<script lang="ts">
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  const services = useServiceCapabilities();
  import RoutingPolicyEditor from '#lib/features/routes/RoutingPolicyEditor.svelte';
  import { apiKeyKeys } from '#lib/features/access/api-keys/apiKeyKeys.js';
  import { overviewKeys } from '#lib/features/overview/overviewKeys.js';

  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import {
    createApiKey,
    getApiKey,
    updateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '#lib/features/access/api-keys/api.js';
  import ApiKeyInventory from '#lib/features/access/api-keys/ApiKeyInventory.svelte';
  import ApiKeyPolicyForm from '#lib/features/access/api-keys/ApiKeyPolicyForm.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import ApiKeySecretDialog from '#lib/features/access/api-keys/ApiKeySecretDialog.svelte';
  import type { ApiKeyListState } from '#lib/features/access/api-keys/apiKeyListState.js';
  import type { ApiKeyPolicyCommand } from '#lib/features/access/api-keys/apiKeyPolicy.js';
  import { isApiKeyUsable } from './apiKeyLifecycle';

  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';

  const createIntent = useMutationIntent();

  let {
    isNew = false,
    keyId,
    listState = $bindable()
  }: {
    isNew?: boolean;
    keyId?: string;
    listState: ApiKeyListState;
  } = $props();

  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));
  let editing = $state<ApiKey | null>(null);
  let busy = $state('');
  let policyDirty = $state(false);
  let policyBusy = $state(false);
  let submitError = $state('');
  let secret = $state<ApiKeySecret | null>(null);
  let secretContext = $state<'created' | 'rotated'>('created');
  let preferredRoute = $state<string | undefined>();
  const loaded = createQuery(() => ({
    queryKey: [...apiKeyKeys.root, 'detail', keyId ?? ''] as const,
    enabled: Boolean(keyId),
    queryFn: ({ signal }) => getApiKey(keyId ?? '', signal)
  }));
  $effect(() => {
    if (loaded.data && editing?.id !== loaded.data.id) edit(loaded.data);
  });
  const isForm = $derived(isNew || editing !== null);
  const canChangeForm = $derived(
    canManage && (!editing || isApiKeyUsable(editing))
  );

  const captureOwner = usePrincipalLifetime(() => {
    secret = null;
    preferredRoute = undefined;
    editing = null;
    policyDirty = policyBusy = false;
    submitError = busy = '';
  });

  function edit(key: ApiKey) {
    policyDirty = policyBusy = false;
    editing = key;
    submitError = '';
  }

  function cancelEdit() {
    if (busy || policyBusy) return;
    void goto(resolve('keys'));
  }

  async function submit(command: ApiKeyPolicyCommand, route?: string) {
    if (!canManage || busy || policyDirty || policyBusy) return false;
    const isCurrent = captureOwner();
    busy = command.kind;
    submitError = '';
    try {
      if (command.kind === 'update') {
        const keyName = command.key.name;
        const result = await updateApiKey(command.key, command.input);
        if (!isCurrent()) return false;
        if (editing?.id === command.key.id)
          editing = { ...editing, etag: result.etag };
        notify(
          services.gatewayAvailable
            ? `Saved ${keyName}. Gateways pick up the change within a few seconds.`
            : `Saved ${keyName}`
        );
      } else {
        const submission = createIntent.prepare<
          Parameters<typeof createApiKey>[0]
        >(command.input);
        const result = await createApiKey(
          submission.input,
          submission.requestId
        );
        if (!isCurrent()) return false;
        secret = result;
        createIntent.reset();
        secretContext = 'created';
        preferredRoute = route;
      }
      try {
        await Promise.all([
          queryClient.invalidateQueries(
            { queryKey: apiKeyKeys.root },
            { throwOnError: true }
          ),
          queryClient.invalidateQueries(
            { queryKey: overviewKeys.root },
            { throwOnError: true }
          )
        ]);
      } catch (cause) {
        if (isCurrent())
          submitError = `API key saved, but refreshing failed. ${errorMessage(cause)}`;
      }
      return isCurrent();
    } catch (error) {
      if (isCurrent()) submitError = errorMessage(error);
      return false;
    } finally {
      if (isCurrent()) busy = '';
    }
  }

  function showRotatedSecret(value: ApiKeySecret, route?: string) {
    secret = value;
    secretContext = 'rotated';
    preferredRoute = route;
  }

  function dismissSecret() {
    secret = null;
    preferredRoute = undefined;
    if (isNew) void goto(resolve('keys'));
  }
</script>

<svelte:head><title>API keys · OpenLLMProxy</title></svelte:head>

{#if secret}
  <ApiKeySecretDialog
    {secret}
    context={secretContext}
    {preferredRoute}
    onClose={dismissSecret}
  />
{/if}

{#if keyId && loaded.isPending}
  <div class="loading-state" role="status">Loading API key…</div>
{:else if keyId && loaded.isError && !loaded.data}
  <Callout tone="danger" title="API key didn’t load">
    {errorMessage(loaded.error)}
    {#snippet actions()}<a
        class="button button-secondary"
        href={resolve('keys')}>All API keys</a
      >{/snippet}
  </Callout>
{:else if isForm}
  <ApiKeyPolicyForm
    {editing}
    busy={busy || (policyBusy ? 'routing' : '')}
    publicationBlocked={policyDirty || policyBusy}
    {submitError}
    canManage={canChangeForm}
    onSubmit={submit}
    onSaved={() => {
      if (keyId && !submitError) void goto(resolve('keys'));
    }}
    onCancel={cancelEdit}
    onClearError={() => (submitError = '')}
  />
{:else}
  <ApiKeyInventory
    bind:listState
    {submitError}
    {canManage}
    onSecret={showRotatedSecret}
  />
{/if}

{#if editing && services.gatewayAvailable}<RoutingPolicyEditor
    scope="api-key"
    id={editing.id}
    resourceEtag={editing.etag}
    canManage={canManage && !busy}
    bind:dirty={policyDirty}
    bind:busy={policyBusy}
    onSaved={(etag, previousEtag) => {
      if (editing?.etag === previousEtag) {
        editing = { ...editing, etag };
      }
    }}
  />{/if}
