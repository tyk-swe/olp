<script lang="ts">
  import { onDestroy } from 'svelte';
  import { importSubscriptionCredentials } from '$lib/features/providers/api/grants';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { getProvider } from '$lib/features/providers/api/providers';
  import { listProviderKinds } from '$lib/features/providers/api/models';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { requiresGrant } from '$lib/features/providers/providerEditor';
  import { GrantEnrollmentState } from '$lib/features/providers/grantEnrollment.svelte';
  import GrantEnrollmentPanel from '$lib/features/providers/GrantEnrollmentPanel.svelte';
  import { useRole } from '$lib/features/access/session/useRole.svelte';

  let {
    providerId,
    allowed,
    onComplete,
    onBusyChange
  }: {
    providerId: string;
    allowed: boolean;
    onComplete: (credentialId: string) => void;
    onBusyChange: (busy: boolean) => void;
  } = $props();
  const client = useQueryClient();
  const provider = createQuery(() => ({
    queryKey: providerKeys.detail(providerId),
    queryFn: ({ signal }) => getProvider(providerId, signal),
    enabled: !!providerId
  }));
  const kinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  const spec = $derived(
    kinds.data?.find((item) => item.kind === provider.data?.configuration.kind)
  );
  const supported = $derived(
    !!provider.data &&
      !!spec &&
      requiresGrant(spec, provider.data.configuration.auth_mode)
  );
  const importProfile = $derived(
    provider.data?.configuration.profile_id === 'codex-subscription'
      ? ({
          id: 'codex-subscription',
          name: 'Codex',
          file: 'Codex auth.json'
        } as const)
      : provider.data?.configuration.profile_id === 'claude-subscription'
        ? ({
            id: 'claude-subscription',
            name: 'Claude',
            file: 'Claude .credentials.json'
          } as const)
        : provider.data?.configuration.profile_id === 'antigravity-subscription'
          ? ({
              id: 'antigravity-subscription',
              name: 'Antigravity',
              file: 'Antigravity migration JSON'
            } as const)
          : null
  );
  const access = useRole();
  const principal = access.user?.id;
  let error = $state('');
  let notice = $state('');
  let importInput = $state('');
  let importing = $state(false);
  let importActive = true;
  onDestroy(() => {
    importActive = false;
  });
  const grant = new GrantEnrollmentState({
    reportTransientErrors: true,
    allowed: () => allowed && access.user?.id === principal,
    error: (message) => (error = message),
    completed: async (result, { secret }) => {
      // The credential write succeeded independently of subsequent refresh.
      onComplete(result.credential_id);
      notice =
        secret &&
        provider.data?.configuration.profile_id !== 'antigravity-subscription'
          ? 'Key enrolled. Save the account to associate this credential. Enrollment did not test the key or inference.'
          : 'Grant enrolled. Save the account to associate this credential. Enrollment did not test inference.';
      await Promise.all([
        client.invalidateQueries(
          {
            queryKey: providerKeys.credentials(providerId)
          },
          { throwOnError: true }
        ),
        client.invalidateQueries(
          { queryKey: providerKeys.detail(providerId) },
          { throwOnError: true }
        )
      ]);
    }
  });
  const enrollmentAllowed = $derived(allowed && access.user?.id === principal);
  $effect(() => onBusyChange(!!grant.busy || !!grant.enrollment || importing));
  $effect(() => {
    if (!enrollmentAllowed) importInput = '';
  });

  async function importCredentials() {
    const selectedImport = importProfile;
    if (
      !enrollmentAllowed ||
      importing ||
      grant.busy ||
      !provider.data ||
      !selectedImport
    )
      return;
    const credentials = importInput;
    importInput = '';
    error = '';
    notice = '';
    try {
      JSON.parse(credentials);
    } catch {
      error = `Paste valid ${selectedImport.file} JSON.`;
      return;
    }
    importing = true;
    try {
      const completion = await importSubscriptionCredentials(
        provider.data,
        credentials,
        selectedImport.id
      );
      if (!importActive || !enrollmentAllowed) return;
      onComplete(completion.credential_id);
      notice =
        'Credentials imported. OLP owns refresh. Stop using the original local credentials. Inference has not been tested.';
      await Promise.all([
        client.invalidateQueries({
          queryKey: providerKeys.credentials(providerId)
        }),
        client.invalidateQueries({ queryKey: providerKeys.detail(providerId) })
      ]);
    } catch {
      if (importActive && enrollmentAllowed)
        error = `Could not import ${selectedImport.name} credentials. Check the supported format and account, then sign in again before retrying.`;
    } finally {
      importing = false;
    }
  }
</script>

<div class="enrollment">
  <p>
    Enroll through the provider's supported grant flow. Use device sign-in for
    Codex, browser sign-in for Claude, or a pasted API key for OpenCode Go and
    the coding plans. Upstream credentials stay in OLP. Developers receive only
    an OLP key
  </p>
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if provider.isPending || kinds.isPending}
    <p role="status">Loading grant capabilities…</p>
  {:else if provider.isError || kinds.isError}
    <p role="alert">
      Could not load grant capabilities. <button
        class="text-button"
        type="button"
        onclick={() => {
          void provider.refetch();
          void kinds.refetch();
        }}>Retry</button
      >
    </p>
  {:else if grant.enrollment}
    <GrantEnrollmentPanel
      profileId={provider.data?.configuration.profile_id}
      enrollment={grant.enrollment}
      bind:input={grant.input}
      busy={grant.busy}
      onContinue={() => grant.continue()}
      onPoll={() => grant.poll()}
      onCancel={() => grant.cancel()}
    />
    {#if error}<button
        class="button button-secondary"
        type="button"
        disabled={!!grant.busy || !allowed}
        onclick={() => grant.poll()}>Check authorization again</button
      >{/if}
  {:else}
    <button
      class="button button-secondary"
      type="button"
      disabled={!enrollmentAllowed || !supported || !!grant.busy || importing}
      onclick={() => {
        if (provider.data) {
          notice = '';
          void grant.start(provider.data);
        }
      }}
    >
      {grant.busy ? 'Starting…' : 'Enroll subscription account'}
    </button>
    {#if supported && importProfile}
      <label for="subscription-credential-import"
        >Import {importProfile.name} credentials once</label
      >
      <p>
        {#if importProfile.id === 'antigravity-subscription'}
          Paste migration JSON with type "antigravity", access_token,
          refresh_token, client_id and client_secret for the original OAuth
          client. The client_id must match this provider. Raw IDE stores and
          credentials missing the original client authorization are unsupported.
        {:else}
          Paste the contents of {importProfile.file} with subscription tokens.
        {/if}
        OLP owns refresh after import. This does not synchronize local files.
      </p>
      <input
        type="password"
        id="subscription-credential-import"
        aria-label={importProfile.file}
        autocomplete="off"
        spellcheck="false"
        bind:value={importInput}
        disabled={!enrollmentAllowed || !!grant.busy || importing}
      />
      <button
        class="button button-secondary"
        type="button"
        disabled={!enrollmentAllowed ||
          !!grant.busy ||
          importing ||
          !importInput.trim()}
        onclick={() => void importCredentials()}
      >
        {importing ? 'Importing…' : `Import ${importProfile.name} credentials`}
      </button>
    {/if}
    {#if provider.data && !supported && !kinds.isPending}<p>
        A supported grant authentication mode must be configured on this
        provider first.
      </p>{/if}
  {/if}
</div>

<style>
  .enrollment {
    display: grid;
    gap: 0.75rem;
  }
  p {
    color: var(--foreground-subtle);
    line-height: 1.6;
  }
  .field-error {
    color: var(--danger);
  }
</style>
