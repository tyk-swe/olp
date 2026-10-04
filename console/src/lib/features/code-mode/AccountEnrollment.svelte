<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { getProvider } from '$lib/features/providers/api/providers';
  import { listProviderKinds } from '$lib/features/providers/api/models';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { requiresGrant } from '$lib/features/providers/providerEditor';
  import {
    startGrantEnrollment,
    continueGrantEnrollment,
    pollGrantEnrollment,
    cancelGrantEnrollment,
    type GrantEnrollment,
    type GrantEnrollmentCompletion
  } from '$lib/features/providers/api/grants';
  import GrantEnrollmentPanel from '$lib/features/providers/GrantEnrollmentPanel.svelte';
  import { ApiProblem, errorMessage } from '$lib/api/http';

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
  let enrollment = $state<GrantEnrollment | null>(null);
  let input = $state('');
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
  $effect(() => onBusyChange(!!busy || !!enrollment));

  async function completed(result: GrantEnrollmentCompletion) {
    enrollment = null;
    input = '';
    await Promise.all([
      client.invalidateQueries({
        queryKey: providerKeys.credentials(providerId)
      }),
      client.invalidateQueries({ queryKey: providerKeys.detail(providerId) })
    ]);
    onComplete(result.credential_id);
    notice =
      'Grant enrolled. Save the account to associate this credential. Enrollment did not test inference.';
  }
  async function run(action: string, work: () => Promise<void>) {
    if (busy || !allowed) return;
    busy = action;
    error = '';
    try {
      await work();
    } catch (e) {
      error = errorMessage(e);
    } finally {
      busy = '';
    }
  }
  async function poll(): Promise<number | null> {
    if (!enrollment || !allowed) return null;
    if (busy) return enrollment.device?.interval ?? null;
    let interval: number | null = null;
    await run('poll', async () => {
      if (!enrollment) return;
      const result = await pollGrantEnrollment(enrollment).catch(
        (e: unknown) => {
          if (
            e instanceof ApiProblem &&
            e.problem.status >= 400 &&
            e.problem.status < 500 &&
            ![408, 429].includes(e.problem.status)
          )
            enrollment = null;
          throw e;
        }
      );
      if (result.completion) await completed(result.completion);
      else if (result.status === 'pending')
        interval = result.interval ?? enrollment.device?.interval ?? null;
      else {
        error = `Grant authorization ${result.status}. Start enrollment again.`;
        enrollment = null;
      }
    });
    return enrollment
      ? (interval ?? enrollment.device?.interval ?? null)
      : null;
  }
</script>

<div class="enrollment">
  <p>
    Enroll through the provider's supported grant flow. Upstream credentials
    stay in OLP; developers receive only an OLP key.
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
  {:else if enrollment}
    <GrantEnrollmentPanel
      {enrollment}
      bind:input
      {busy}
      onContinue={() =>
        run('continue', async () => {
          if (enrollment)
            await completed(await continueGrantEnrollment(enrollment, input));
        })}
      onPoll={poll}
      onCancel={() =>
        run('cancel', async () => {
          if (enrollment) await cancelGrantEnrollment(enrollment);
          enrollment = null;
          input = '';
        })}
    />
    {#if error}<button
        class="button button-secondary"
        type="button"
        disabled={!!busy || !allowed}
        onclick={poll}>Check authorization again</button
      >{/if}
  {:else}
    <button
      class="button button-secondary"
      type="button"
      disabled={!allowed || !supported || !!busy}
      onclick={() =>
        run('start', async () => {
          if (provider.data)
            enrollment = await startGrantEnrollment(provider.data);
        })}
    >
      {busy ? 'Starting…' : 'Enroll subscription account'}
    </button>
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
