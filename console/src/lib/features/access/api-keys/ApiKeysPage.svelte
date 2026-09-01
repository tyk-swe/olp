<script lang="ts">
  import { goto } from '$app/navigation';
  import { queryKeys } from '$lib/api/queryKeys';
  import { resolve } from '$app/paths';
  import { onDestroy } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/auth/useRole.svelte';
  import {
    createApiKey,
    updateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '$lib/api/management/api-keys';
  import ApiKeyInventory from './ApiKeyInventory.svelte';
  import ApiKeyPolicyForm from './ApiKeyPolicyForm.svelte';
  import ApiKeySecretDialog from './ApiKeySecretDialog.svelte';
  import type { ApiKeyListState } from './apiKeyListState';
  import type { ApiKeyPolicyInput } from './apiKeyPolicy';

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
  let submitError = $state('');
  let notice = $state('');
  let secret = $state<ApiKeySecret | null>(null);
  let secretContext = $state<'created' | 'rotated'>('created');
  let preferredRoute = $state<string | undefined>();
  const isForm = $derived(isNew || editing !== null);
  const canChangeForm = $derived(
    canManage &&
      (!editing ||
        (!editing.revoked_at &&
          (!editing.expires_at || new Date(editing.expires_at) >= new Date())))
  );

  onDestroy(() => {
    secret = null;
  });

  function edit(key: ApiKey) {
    editing = key;
    submitError = notice = '';
  }

  function cancelEdit() {
    editing = null;
    submitError = '';
  }

  async function submit(input: ApiKeyPolicyInput, route?: string) {
    if (!canManage) return false;
    busy = editing ? 'update' : 'create';
    submitError = notice = '';
    try {
      if (editing) {
        const keyName = editing.name;
        await updateApiKey(editing, input);
        editing = null;
        notice = `${keyName} policy updated. Gateways will converge on the new runtime generation.`;
      } else {
        secret = await createApiKey(input);
        secretContext = 'created';
        preferredRoute = route;
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.apiKeys.root })
      ]);
      return true;
    } catch (error) {
      submitError = errorMessage(error);
      return false;
    } finally {
      busy = '';
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
    {busy}
    {submitError}
    canManage={canChangeForm}
    onSubmit={submit}
    onCancel={cancelEdit}
    onClearError={() => (submitError = '')}
  />
{:else}
  <ApiKeyInventory
    {listState}
    {notice}
    {submitError}
    {canManage}
    onEdit={edit}
    onSecret={showRotatedSecret}
  />
{/if}
