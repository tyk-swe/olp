<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import { listProviders } from '$lib/features/providers/api/providers';
  import { listProviderCredentials } from '$lib/features/providers/api/credentials';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import {
    saveCodeAccount,
    type CodeAccount,
    type CodeAccountWrite
  } from '$lib/api/code-mode';
  import { nativeModels } from './presentation';
  import AccountEnrollment from './AccountEnrollment.svelte';

  import {
    useResourceSubmission,
    type ResourceEditorProps
  } from './resourceSubmission.svelte';
  import ResourceEditorForm from './ResourceEditorForm.svelte';

  let {
    current,
    projectId,
    allowed,
    onDraftChange,
    onSaved,
    onCancel
  }: ResourceEditorProps<CodeAccount> = $props();
  const initial = untrack(() => current);
  let name = $state(initial?.name ?? '');
  let providerId = $state(initial?.provider_id ?? '');
  let credentialId = $state(initial?.credential_id ?? '');
  let models = $state(initial?.models.join('\n') ?? '');
  let enabled = $state(initial?.enabled ?? true);
  let enrolling = $state(false);

  const providers = createQuery(() => ({
    queryKey: providerKeys.all(),
    queryFn: ({ signal }) => listProviders(signal)
  }));
  const credentials = createQuery(() => ({
    queryKey: providerKeys.credentials(providerId),
    queryFn: ({ signal }) => listProviderCredentials(providerId, signal),
    enabled: !!providerId
  }));
  const submission = useResourceSubmission({
    allowed: () => allowed,
    creating: () => !current,
    onDraftChange: (dirty) => onDraftChange(dirty),
    onSaved: () => onSaved(),
    onCancel: () => onCancel(),
    blocked: () => enrolling,
    build: (): CodeAccountWrite => ({
      project_id: projectId,
      name,
      provider_id: providerId,
      credential_id: credentialId,
      models: nativeModels(models),
      enabled
    }),
    persist: (input, requestId) => saveCodeAccount(input, current, requestId)
  });
</script>

{#snippet enrollment()}
  {#if providerId}
    {#key providerId}<AccountEnrollment
        {providerId}
        allowed={allowed && !submission.busy && !submission.submitted}
        onComplete={(id) => {
          credentialId = id;
          submission.markDirty();
        }}
        onBusyChange={(value) => (enrolling = value)}
      />{/key}
  {/if}
{/snippet}

<ResourceEditorForm
  title="account"
  creating={!current}
  {allowed}
  {submission}
  referenceError={providers.error || credentials.error}
  {enrolling}
  {enrollment}
>
  <label for="code-name">Name</label><input
    id="code-name"
    bind:value={name}
    required
    maxlength="100"
  />
  <label for="code-provider">Provider</label>
  <select
    id="code-provider"
    bind:value={providerId}
    disabled={!!current || enrolling}
    required
    onchange={() => (credentialId = '')}
  >
    <option value="">Choose a provider</option>
    {#each (providers.data ?? []).filter((p) => p.project_id === projectId) as provider (provider.id)}<option
        value={provider.id}>{provider.name}</option
      >{/each}
    {#if providerId && !providers.data?.some((p) => p.id === providerId && p.project_id === projectId)}
      <option value={providerId}>{providerId} · current selection</option>
    {/if}
  </select>
  <p>
    <a href={resolve('/providers')}>Manage providers and grant authentication</a
    >. Code-mode does not require an inference probe or ordinary provider
    activation.
  </p>
  <label for="code-credential">Enrolled credential version</label>
  <select
    id="code-credential"
    bind:value={credentialId}
    required
    disabled={enrolling}
  >
    <option value="">Choose an enrolled grant</option>
    {#each (credentials.data ?? []).filter((c) => c.grant) as credential (credential.id)}
      <option value={credential.id}
        >Version {credential.version} · {credential.grant?.principal}
        {credential.revoked_at ? '· revoked' : ''}</option
      >
    {/each}
    {#if credentialId && !credentials.data?.some((c) => c.id === credentialId)}<option
        value={credentialId}>{credentialId} · current selection</option
      >{/if}
  </select>
  <p>
    Replacing a credential must preserve the observed principal. Existing trees
    never switch accounts.
  </p>
  <label for="code-models">Native models</label><textarea
    id="code-models"
    bind:value={models}
    required
    rows="3"
    placeholder="One native model identifier per line"></textarea>
  <p>
    Model names pass through unchanged. An allowed model is not proof of client
    or inference qualification.
  </p>
  <label class="choice"
    ><input type="checkbox" bind:checked={enabled} />Enabled</label
  >
</ResourceEditorForm>
