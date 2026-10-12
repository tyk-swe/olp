<script lang="ts">
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  const services = useServiceCapabilities();
  import RoutingPolicyEditor from '$lib/features/routes/RoutingPolicyEditor.svelte';
  import { apiKeyKeys } from '$lib/features/access/api-keys/apiKeyKeys';
  import { overviewKeys } from '$lib/features/overview/overviewKeys';

  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    createApiKey,
    updateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '$lib/features/access/api-keys/api';
  import ApiKeyInventory from '$lib/features/access/api-keys/ApiKeyInventory.svelte';
  import ApiKeyPolicyForm from '$lib/features/access/api-keys/ApiKeyPolicyForm.svelte';
  import BudgetGroupsPanel from '$lib/features/access/budget-groups/BudgetGroupsPanel.svelte';
  import NotificationsPanel from '$lib/features/access/notifications/NotificationsPanel.svelte';
  import ApiKeySecretDialog from '$lib/features/access/api-keys/ApiKeySecretDialog.svelte';
  import type { ApiKeyListState } from '$lib/features/access/api-keys/apiKeyListState';
  import type { ApiKeyPolicyInput } from '$lib/features/access/api-keys/apiKeyPolicy';
  import { isApiKeyUsable } from './apiKeyLifecycle';

  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/forms/mutationIntent.svelte';

  const createIntent = useMutationIntent();

  let {
    isNew = false,
    listState = $bindable()
  }: {
    isNew?: boolean;
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
  let notice = $state('');
  let secret = $state<ApiKeySecret | null>(null);
  let secretContext = $state<'created' | 'rotated'>('created');
  let preferredRoute = $state<string | undefined>();
  const isForm = $derived(isNew || editing !== null);
  const canChangeForm = $derived(
    canManage && (!editing || isApiKeyUsable(editing))
  );

  const captureOwner = usePrincipalLifetime(() => {
    secret = null;
    preferredRoute = undefined;
    editing = null;
    policyDirty = policyBusy = false;
    submitError = notice = busy = '';
  });

  function edit(key: ApiKey) {
    policyDirty = policyBusy = false;
    editing = key;
    submitError = notice = '';
  }

  function cancelEdit() {
    if (busy || policyBusy) return;
    editing = null;
    policyDirty = false;
    submitError = '';
  }

  async function submit(input: ApiKeyPolicyInput, route?: string) {
    if (!canManage || busy || policyDirty || policyBusy) return false;
    const isCurrent = captureOwner();
    busy = editing ? 'update' : 'create';
    submitError = notice = '';
    try {
      if (editing) {
        const keyName = editing.name;
        await updateApiKey(editing, input);
        if (!isCurrent()) return false;
        editing = null;
        notice = services.gatewayAvailable
          ? `${keyName} policy updated. Gateways will converge on the new runtime generation.`
          : `${keyName} policy saved.`;
      } else {
        const submission =
          createIntent.prepare<Parameters<typeof createApiKey>[0]>(input);
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
    if (isNew) void goto(resolve('/api-keys'));
  }
</script>

<svelte:head><title>API Keys · OpenLLMProxy</title></svelte:head>

{#if secret}
  <ApiKeySecretDialog
    {secret}
    context={secretContext}
    {preferredRoute}
    onClose={dismissSecret}
  />
{/if}

{#if isForm}
  <ApiKeyPolicyForm
    {editing}
    busy={busy || (policyBusy ? 'routing' : '')}
    publicationBlocked={policyDirty || policyBusy}
    {submitError}
    canManage={canChangeForm}
    onSubmit={submit}
    onCancel={cancelEdit}
    onClearError={() => (submitError = '')}
  />
{:else}
  <ApiKeyInventory
    bind:listState
    {notice}
    {submitError}
    {canManage}
    onEdit={edit}
    onSecret={showRotatedSecret}
  />
  <BudgetGroupsPanel />
  <NotificationsPanel />
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
