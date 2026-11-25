<script lang="ts">
  import UnconfinedReview from './UnconfinedReview.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { formatBytes, shortDigest } from '#lib/format.js';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import {
    beginIdentityReauthentication,
    listOidcIdentities,
    ReauthenticateDialog,
    reauthenticateWithPassword
  } from '#lib/features/identity/index.js';
  import { pluginKeys } from '#lib/features/plugins/pluginKeys.js';
  import {
    listUnconfinedExecutables,
    needsReauthentication,
    permitUnconfinedPlugin,
    pluginProblem,
    pluginTitle,
    reviewUnconfinedExecutable,
    type UnconfinedExecutableReview
  } from '#lib/features/plugins/api.js';

  let {
    enabled,
    canManage,
    onPermitted
  }: {
    enabled: boolean;
    canManage: boolean;
    onPermitted: () => Promise<unknown>;
  } = $props();

  const VERIFIED_FOR_MS = 5 * 60 * 1000;

  const executables = createQuery(() => ({
    queryKey: pluginKeys.unconfined(),
    queryFn: ({ signal }) => listUnconfinedExecutables(signal),
    enabled: enabled && canManage
  }));

  let review = $state<UnconfinedExecutableReview | null>(null);
  let acknowledged = $state(false);
  const pluginAction = useAction();
  const reauthAction = useAction();
  let reviewing = $state('');
  const busy = $derived(reviewing || pluginAction.key);
  const reauthenticationBusy = $derived(reauthAction.busy);
  let error = $state('');
  let notice = $state('');
  let reauthenticating = $state(false);
  let reauthenticationError = $state('');
  let singleSignOn = $state(false);

  let verified = $state(false);
  let verifiedExpiry: ReturnType<typeof setTimeout> | undefined;

  onMount(() => {
    const search = new URLSearchParams(window.location.search);
    if (search.get('reauthenticated') === 'plugin_permit') {
      goto(resolve('plugins'), { shallow: true, replace: true });
      // The query parameter is only a hint to show; the permission itself
      // still needs the owner's review and acknowledgement.
      verified = true;

      notice =
        'Identity verified. Review the executable and permit it within five minutes.';
      verifiedExpiry = setTimeout(() => (verified = false), VERIFIED_FOR_MS);
    }
    return () => clearTimeout(verifiedExpiry);
  });

  async function reviewExecutable(name: string) {
    reviewing = `review-${name}`;
    error = notice = '';
    review = null;
    acknowledged = false;
    const result = await readResult(() => reviewUnconfinedExecutable(name));
    reviewing = '';
    if (result.ok) review = result.value;
    else error = pluginProblem(result.error);
  }

  async function permit() {
    if (!review || !acknowledged) return;
    if (verified) await submitPermission();
    else await reauthenticate();
  }

  async function reauthenticate() {
    error = '';
    await pluginAction.run(
      async () => {
        const identities = await listOidcIdentities();
        if (identities.has_local_password)
          return { kind: 'password' as const, identities };
        return {
          kind: 'sso' as const,
          redirect: await beginIdentityReauthentication('plugin_permit')
        };
      },
      (result, current) => {
        if (!current()) return;
        if (result.kind === 'password') {
          singleSignOn =
            result.identities.oidc_reauthentication_available === true;
          reauthenticationError = '';
          reauthenticating = true;
          return;
        }
        window.location.assign(result.redirect);
      },
      { key: 'permit', failure: (cause) => errorMessage(cause) }
    );
    error = pluginAction.error;
  }

  async function confirmWithPassword(password: string) {
    reauthenticationError = '';
    const ok = await reauthAction.run(
      () => reauthenticateWithPassword(password, 'plugin_permit'),
      undefined,
      { key: 'password', failure: (cause) => errorMessage(cause) }
    );
    if (!ok) {
      reauthenticationError = reauthAction.error;
      return;
    }
    reauthenticating = false;
    await submitPermission();
  }

  async function confirmWithSingleSignOn() {
    reauthenticationError = '';
    await reauthAction.run(
      async () => {
        window.location.assign(
          await beginIdentityReauthentication('plugin_permit')
        );
      },
      undefined,
      { key: 'sso', failure: (cause) => errorMessage(cause) }
    );
    reauthenticationError = reauthAction.error;
  }

  async function submitPermission() {
    const reviewed = review;
    if (!reviewed) return;
    error = notice = '';
    let failureCause: unknown;
    const committed = await pluginAction.run(
      () => permitUnconfinedPlugin(reviewed),
      async (plugin, current) => {
        verified = false;
        clearTimeout(verifiedExpiry);
        review = null;
        acknowledged = false;
        await Promise.all([executables.refetch(), onPermitted()]);
        if (!current()) return;
        notice = `Permitted ${pluginTitle(plugin)}. Providers may now use its profiles.`;
      },
      {
        key: 'permit',
        failure: (cause) => {
          failureCause = cause;
          return pluginProblem(cause);
        }
      }
    );
    if (!committed && needsReauthentication(failureCause)) {
      verified = false;
      await reauthenticate();
      return;
    }
    error = pluginAction.error;
  }
</script>

<section class="card unconfined" aria-labelledby="unconfined-heading">
  <header>
    <div>
      <p class="eyebrow">Experimental</p>
      <h2 id="unconfined-heading">Unconfined plugins</h2>
    </div>
    <span class="badge" class:warning={enabled}
      >{enabled ? 'Enabled' : 'Disabled'}</span
    >
  </header>
  {#if !enabled}
    <p class="muted">
      This deployment runs plugins confined only. Only a deployment setting,
      <code>OLP_UNCONFINED_PLUGIN_DIR</code>, enables unconfined plugins; the
      console and the management API can't.
    </p>
  {:else}
    <p class="muted">
      This deployment enables unconfined plugins: native executables in its
      unconfined plugin directory, which run as subprocesses of OpenLLMProxy
      with its privileges. An owner reviews and permits each build.
    </p>
    {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
    {#if notice}<Callout tone="success">{notice}</Callout>{/if}
    {#if !canManage}
      <ReadOnlyNote
        >Only owners can review and permit unconfined plugins.</ReadOnlyNote
      >
    {:else if executables.isPending}
      <div class="loading-state" role="status">Loading executables…</div>
    {:else if executables.isError}
      <div class="inline-problem" role="alert">
        {errorMessage(executables.error)}
        <button
          class="text-button"
          type="button"
          onclick={() => executables.refetch()}>Retry</button
        >
      </div>
    {:else if !executables.data?.length}
      <p class="muted">The unconfined plugin directory holds no executables.</p>
    {:else}
      <ScrollTable label="Unconfined plugin executables">
        <table class="data-table">
          <thead
            ><tr
              ><th>Executable</th><th>Digest</th><th>Size</th><th>Status</th><th
                ><span class="sr-only">Actions</span></th
              ></tr
            ></thead
          >
          <tbody>
            {#each executables.data as file (file.name)}
              <tr>
                <td><code>{file.name}</code></td>
                <td
                  ><code title={file.digest}>{shortDigest(file.digest)}</code
                  ></td
                >
                <td>{formatBytes(file.size_bytes)}</td>
                <td
                  ><span
                    class="badge"
                    class:success={file.permitted}
                    class:warning={!file.permitted}
                    >{file.permitted ? 'Permitted' : 'Not permitted'}</span
                  ></td
                >
                <td class="row-action">
                  {#if !file.permitted}<button
                      class="button button-secondary"
                      type="button"
                      aria-label={`Review ${file.name}`}
                      disabled={Boolean(busy)}
                      onclick={() => reviewExecutable(file.name)}
                      >{busy === `review-${file.name}`
                        ? 'Reviewing…'
                        : 'Review'}</button
                    >{/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    {/if}
    {#if review}<UnconfinedReview
        {review}
        bind:acknowledged
        {busy}
        {permit}
        oncancel={() => (review = null)}
      />{/if}
  {/if}
</section>

{#if reauthenticating}
  <ReauthenticateDialog
    title="Confirm the permission"
    description="An unconfined plugin runs with OpenLLMProxy's privileges, so confirm your identity before permitting it."
    busy={reauthenticationBusy}
    error={reauthenticationError}
    onOidc={singleSignOn ? confirmWithSingleSignOn : undefined}
    onConfirm={confirmWithPassword}
    onCancel={() => (reauthenticating = false)}
  />
{/if}

<style>
  .unconfined {
    display: grid;
    gap: 1rem;
    margin-top: 1rem;
    padding: 1.5rem;
    min-width: 0;
  }
  .unconfined header {
    display: flex;
    align-items: start;
    justify-content: space-between;
    gap: 1rem;
  }
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .muted,
  .row-action {
    text-align: right;
  }
</style>
