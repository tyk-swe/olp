<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import {
    getProvider,
    listProviders
  } from '#lib/features/providers/providers.js';
  import { listProviderCredentials } from '#lib/features/providers/credentials.js';
  import { providerKeys } from '#lib/features/providers/providerKeys.js';
  import {
    saveCodeAccount,
    type CodeAccount,
    type CodeAccountWrite
  } from '#lib/features/code-mode/api.js';

  import {
    extraUsageAdapterFor,
    extraUsageValue,
    nativeModels,
    providerProfilePending,
    showsExtraUsage
  } from './presentation';

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
  let cycleAnchor = $state(initial?.cycle_anchor ?? '');
  let enabled = $state(initial?.enabled ?? true);
  let extraUsage = $state(
    initial?.extra_usage_enabled == null
      ? ''
      : String(initial.extra_usage_enabled)
  );
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
  const provider = createQuery(() => ({
    queryKey: providerKeys.detail(providerId),
    queryFn: ({ signal }) => getProvider(providerId, signal),
    enabled: !!providerId
  }));
  const profilePending = $derived(
    providerProfilePending(!initial, providerId, provider.data !== undefined)
  );
  const extraUsageAdapter = $derived(
    extraUsageAdapterFor(
      initial?.adapter,
      !initial,
      provider.data?.configuration.profile_id
    )
  );
  const qwenTokenPlan = $derived(
    initial?.adapter === 'qwen_token_plan' ||
      provider.data?.configuration.profile_id === 'qwen-token-plan' ||
      provider.data?.configuration.profile_id === 'qwen-token-plan-cn'
  );
  const submission = useResourceSubmission({
    allowed: () => allowed,
    creating: () => !current,
    onDraftChange: (dirty) => onDraftChange(dirty),
    onSaved: () => onSaved(),
    onCancel: () => onCancel(),
    blocked: () => enrolling || profilePending,
    build: (): CodeAccountWrite => ({
      project_id: projectId,
      name,
      provider_id: providerId,
      credential_id: credentialId,
      models: nativeModels(models),
      cycle_anchor: qwenTokenPlan && cycleAnchor ? cycleAnchor : null,
      enabled,
      extra_usage_enabled: extraUsageValue(
        extraUsageAdapter,
        extraUsage,
        initial?.extra_usage_enabled
      )
    }),
    persist: (input, requestId) =>
      saveCodeAccount(
        current
          ? { kind: 'update', body: input, current }
          : { kind: 'create', body: input, requestId }
      )
  });
</script>

{#snippet enrollment()}
  {#if providerId}
    {#key providerId}
      <AccountEnrollment
        {providerId}
        allowed={allowed && !submission.busy && !submission.submitted}
        onComplete={(id) => {
          credentialId = id;
          submission.markDirty();
        }}
        onBusyChange={(value) => (enrolling = value)}
      />
    {/key}
  {/if}
{/snippet}

<ResourceEditorForm
  title="account"
  creating={!current}
  {allowed}
  {submission}
  referenceError={providers.error || credentials.error || provider.error}
  {enrolling}
  saveBlocked={profilePending}
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
    <a href={resolve('providers')}>Manage providers and grant authentication</a>
    . Code-mode does not require an inference probe or ordinary provider activation.
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
  {#if qwenTokenPlan}
    <label for="code-cycle-anchor">Subscription cycle anchor</label><input
      id="code-cycle-anchor"
      type="date"
      bind:value={cycleAnchor}
    />
    <p>
      Optional. The monthly quota reset is reported as the next monthly
      anniversary of this date. Without an anchor no reset is reported.
    </p>
  {/if}
  <label class="choice"
    ><input type="checkbox" bind:checked={enabled} />Enabled</label
  >
  {#if showsExtraUsage(extraUsageAdapter)}
    <label for="code-extra-usage">Extra Usage</label>
    <select id="code-extra-usage" bind:value={extraUsage}>
      <option value="">Not recorded</option>
      <option value="true">On</option>
      <option value="false">Off</option>
    </select>
    <p>
      Extra Usage is a paid fallback beyond the plan quota. Record the member
      setting for this Kimi account so paid use is never read as spare included
      allowance
    </p>
  {/if}
</ResourceEditorForm>
