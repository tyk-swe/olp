<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  const services = useServiceCapabilities();
  import { apiKeyKeys } from '#lib/domain/api-keys/apiKeyKeys.js';
  import { overviewKeys } from '#lib/features/overview/index.js';

  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { readResult } from '#lib/editing/result.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    createApiKey,
    updateApiKey,
    type ApiKeySecret
  } from '#lib/features/access/api-keys/api.js';
  import { getApiKey, type ApiKey } from '#lib/domain/api-keys/api.js';
  import ApiKeyInventory from '#lib/features/access/api-keys/ApiKeyInventory.svelte';
  import ApiKeyPolicyForm from '#lib/features/access/api-keys/ApiKeyPolicyForm.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import ApiKeySecretDialog from '#lib/features/access/api-keys/ApiKeySecretDialog.svelte';
  import type { ApiKeyListState } from '#lib/features/access/api-keys/apiKeyListState.js';
  import type { ApiKeyPolicyCommand } from '#lib/features/access/api-keys/apiKeyPolicy.js';
  import { isApiKeyUsable } from './apiKeyLifecycle';

  import { useMutation } from '#lib/editing/action.svelte.js';
  import type { Component } from 'svelte';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';

  const write = useMutation();

  let {
    isNew = false,
    keyId,
    listState = $bindable(),
    RoutingPolicyEditor
  }: {
    isNew?: boolean;
    keyId?: string;
    listState: ApiKeyListState;
    RoutingPolicyEditor: Component<
      {
        scope: 'api-key';
        id: string;
        resourceEtag: string;
        canManage: boolean;
        dirty: boolean;
        busy: boolean;
        onSaved: (etag: string, previousEtag: string) => void;
      },
      object,
      'dirty' | 'busy'
    >;
  } = $props();

  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));
  let editing = $state<ApiKey | null>(null);
  let policyDirty = $state(false);
  let policyBusy = $state(false);
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

  usePrincipalLifetime(() => {
    secret = null;
    preferredRoute = undefined;
    editing = null;
    policyDirty = policyBusy = false;
  });

  function edit(key: ApiKey) {
    policyDirty = policyBusy = false;
    editing = key;
    write.error = '';
  }

  function cancelEdit() {
    if (write.busy || policyBusy) return;
    void goto(resolve('keys'));
  }

  async function refreshSaved(current: () => boolean) {
    const refreshed = await readResult(() =>
      Promise.all([
        queryClient.invalidateQueries(
          { queryKey: apiKeyKeys.root },
          { throwOnError: true }
        ),
        queryClient.invalidateQueries(
          { queryKey: overviewKeys.root },
          { throwOnError: true }
        )
      ])
    );
    if (!refreshed.ok && current())
      write.error = `API key saved, but refreshing failed. ${errorMessage(refreshed.error)}`;
  }

  function submit(command: ApiKeyPolicyCommand, route?: string) {
    if (!canManage || write.busy || policyDirty || policyBusy)
      return Promise.resolve(false);
    if (command.kind === 'update') {
      const keyName = command.key.name;
      return write.run(
        () => updateApiKey(command.key, command.input),
        async (result, current) => {
          if (editing?.id === command.key.id)
            editing = {
              ...editing,
              etag: result.etag,
              ...(command.input.expires_at !== undefined
                ? { expires_at: command.input.expires_at }
                : {})
            };
          notify(
            services.gatewayAvailable
              ? `Saved ${keyName}. Gateways pick up the change within a few seconds.`
              : `Saved ${keyName}`
          );
          await refreshSaved(current);
        },
        { key: 'update' }
      );
    }
    return write.submit(
      command.input,
      (input, requestId) => createApiKey(input, requestId),
      async (result, current) => {
        secret = result;
        secretContext = 'created';
        preferredRoute = route;
        await refreshSaved(current);
      },
      { key: 'create' }
    );
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

<PageTitle title="API keys" />

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
        href={resolve('keys')}
      >
        All API keys
      </a>{/snippet}
  </Callout>
{:else if isForm}
  <ApiKeyPolicyForm
    {editing}
    overlaps={loaded.data?.active_overlaps}
    busy={write.key || (policyBusy ? 'routing' : '')}
    publicationBlocked={policyDirty || policyBusy}
    submitError={write.error}
    canManage={canChangeForm}
    onSubmit={submit}
    onSaved={() => {
      if (keyId && !write.error) void goto(resolve('keys'));
    }}
    onCancel={cancelEdit}
    onClearError={() => (write.error = '')}
  />
{:else}
  <ApiKeyInventory
    bind:listState
    submitError={write.error}
    {canManage}
    onSecret={showRotatedSecret}
  />
{/if}

{#if editing && services.gatewayAvailable}<RoutingPolicyEditor
    scope="api-key"
    id={editing.id}
    resourceEtag={editing.etag}
    canManage={canManage && !write.busy}
    bind:dirty={policyDirty}
    bind:busy={policyBusy}
    onSaved={(etag, previousEtag) => {
      if (editing?.etag === previousEtag) {
        editing = { ...editing, etag };
      }
    }}
  />{/if}
