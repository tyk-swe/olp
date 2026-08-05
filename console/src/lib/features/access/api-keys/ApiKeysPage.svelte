<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { onDestroy } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
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
  let editing = $state<ApiKey | null>(null);
  let busy = $state('');
  let errorMessage = $state('');
  let notice = $state('');
  let secret = $state<ApiKeySecret | null>(null);
  let secretContext = $state<'created' | 'rotated'>('created');
  let preferredRoute = $state<string | undefined>();
  const isForm = $derived(isNew || editing !== null);

  onDestroy(() => {
    secret = null;
  });

  function message(error: unknown) {
    return error instanceof Error
      ? error.message
      : 'The control API could not complete the request.';
  }

  function edit(key: ApiKey) {
    editing = key;
    errorMessage = notice = '';
  }

  function cancelEdit() {
    editing = null;
    errorMessage = '';
  }

  async function submit(input: ApiKeyPolicyInput, route?: string) {
    busy = editing ? 'update' : 'create';
    errorMessage = notice = '';
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
        queryClient.invalidateQueries({ queryKey: ['api-keys'] }),
        queryClient.invalidateQueries({ queryKey: ['api-key-page'] })
      ]);
    } catch (error) {
      errorMessage = message(error);
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
    {errorMessage}
    onSubmit={submit}
    onCancel={cancelEdit}
    onClearError={() => (errorMessage = '')}
  />
{:else}
  <ApiKeyInventory
    {listState}
    {notice}
    {errorMessage}
    onEdit={edit}
    onSecret={showRotatedSecret}
  />
{/if}
