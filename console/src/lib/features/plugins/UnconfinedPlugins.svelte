<script lang="ts">
  import { onMount } from 'svelte';
  import { replaceState } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { formatBytes } from '$lib/format';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import ReauthenticateDialog from '$lib/components/ReauthenticateDialog.svelte';
  import {
    beginOidcReauthentication,
    listOidcIdentities,
    reauthenticateWithPassword
  } from '$lib/features/access/profile/api';
  import { pluginKeys } from '$lib/features/plugins/pluginKeys';
  import {
    listUnconfinedExecutables,
    needsReauthentication,
    permitUnconfinedPlugin,
    pluginProblem,
    reviewUnconfinedExecutable,
    shortDigest,
    type UnconfinedExecutableReview
  } from '$lib/features/plugins/api';

  let {
    enabled,
    canManage,
    onPermitted
  }: {
    // Whether the deployment enables the unconfined tier.
    enabled: boolean;
    canManage: boolean;
    onPermitted: () => Promise<unknown>;
  } = $props();

  // How long a recent authentication authorizes a permission.
  const VERIFIED_FOR_MS = 5 * 60 * 1000;

  const executables = createQuery(() => ({
    queryKey: pluginKeys.unconfined(),
    queryFn: ({ signal }) => listUnconfinedExecutables(signal),
    enabled: enabled && canManage
  }));

  let review = $state<UnconfinedExecutableReview | null>(null);
  const reviewTitle = $derived(
    review ? `${review.manifest.name} ${review.manifest.version}` : ''
  );
  let acknowledged = $state(false);
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
  let reauthenticating = $state(false);
  let reauthenticationBusy = $state(false);
  let reauthenticationError = $state('');
  let singleSignOn = $state(false);
  // Set while single sign-on has verified the owner for a permission, which
  // the next permission spends.
  let verified = $state(false);
  let verifiedExpiry: ReturnType<typeof setTimeout> | undefined;

  onMount(() => {
    const search = new URLSearchParams(window.location.search);
    if (search.get('reauthenticated') === 'plugin_permit') {
      replaceState(resolve('/plugins'), {});
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
    busy = `review-${name}`;
    error = notice = '';
    review = null;
    acknowledged = false;
    try {
      review = await reviewUnconfinedExecutable(name);
    } catch (cause) {
      error = pluginProblem(cause);
    } finally {
      busy = '';
    }
  }

  async function permit() {
    if (!review || !acknowledged) return;
    if (verified) await submitPermission();
    else await reauthenticate();
  }

  /**
   * Asks the owner to confirm their identity: with their password when they
   * have one, else through single sign-on, which returns to this page.
   */
  async function reauthenticate() {
    busy = 'permit';
    error = '';
    try {
      const identities = await listOidcIdentities();
      if (identities.has_local_password) {
        singleSignOn = identities.oidc_reauthentication_available === true;
        reauthenticationError = '';
        reauthenticating = true;
        return;
      }
      window.location.assign(await beginOidcReauthentication('plugin_permit'));
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = '';
    }
  }

  async function confirmWithPassword(password: string) {
    reauthenticationBusy = true;
    reauthenticationError = '';
    try {
      await reauthenticateWithPassword(password, 'plugin_permit');
    } catch (cause) {
      reauthenticationError = errorMessage(cause);
      return;
    } finally {
      reauthenticationBusy = false;
    }
    reauthenticating = false;
    await submitPermission();
  }

  async function confirmWithSingleSignOn() {
    reauthenticationBusy = true;
    reauthenticationError = '';
    try {
      window.location.assign(await beginOidcReauthentication('plugin_permit'));
    } catch (cause) {
      reauthenticationError = errorMessage(cause);
    } finally {
      reauthenticationBusy = false;
    }
  }

  async function submitPermission() {
    const reviewed = review;
    if (!reviewed) return;
    busy = 'permit';
    error = notice = '';
    try {
      const plugin = await permitUnconfinedPlugin(reviewed);
      verified = false;
      clearTimeout(verifiedExpiry);
      review = null;
      acknowledged = false;
      await Promise.all([executables.refetch(), onPermitted()]);
      notice = `Permitted ${plugin.manifest.name} ${plugin.manifest.version}. Providers may now use its profiles.`;
    } catch (cause) {
      if (needsReauthentication(cause)) {
        verified = false;
        await reauthenticate();
        return;
      }
      error = pluginProblem(cause);
    } finally {
      busy = '';
    }
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
    {#if notice}<div class="success-banner" role="status">{notice}</div>{/if}
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
      <div class="table-shell">
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
      </div>
    {/if}
    {#if review}
      <section class="review" aria-labelledby="unconfined-review-heading">
        <h3 id="unconfined-review-heading">Review {reviewTitle}</h3>
        {#if review.manifest.description}<p>
            {review.manifest.description}
          </p>{/if}
        <dl class="facts">
          <div>
            <dt>Executable</dt>
            <dd><code>{review.name}</code></dd>
          </div>
          <div>
            <dt>Digest</dt>
            <dd><code class="digest">{review.digest}</code></dd>
          </div>
          <div>
            <dt>ABI</dt>
            <dd>{review.abi_version}</dd>
          </div>
        </dl>
        <div class="table-shell">
          <table class="data-table">
            <thead
              ><tr
                ><th>Profile</th><th>Label</th><th>Dialect</th><th>Address</th
                ></tr
              ></thead
            >
            <tbody>
              {#each review.manifest.profiles as profile (profile.id)}
                <tr
                  ><td><code>{profile.id}</code></td><td>{profile.label}</td><td
                    ><code>{profile.dialect}</code></td
                  ><td
                    ><code class="address">{profile.hosting.address}</code></td
                  ></tr
                >
              {/each}
            </tbody>
          </table>
        </div>
        <p class="muted">
          Declared origins:
          {#each review.manifest.origins as origin, index (origin)}{index
              ? ', '
              : ''}<code>{origin}</code>{:else}none{/each}.
        </p>
        <div class="risk" role="note">
          <strong>High risk.</strong> An unconfined plugin runs outside every confinement,
          with the operating system privileges of each OpenLLMProxy process that serves
          its providers. It receives their credentials and requests, can read whatever
          those processes can, reaches any network and bypasses the egress policy.
          Permit only an executable you trust as much as OpenLLMProxy itself.
        </div>
        <label class="acknowledge"
          ><input type="checkbox" bind:checked={acknowledged} /> I understand
          that {review.name} will run unconfined, with OpenLLMProxy's privileges.</label
        >
        <div class="actions">
          <button
            class="button button-primary"
            type="button"
            disabled={!acknowledged || Boolean(busy)}
            onclick={permit}
            >{busy === 'permit'
              ? 'Permitting…'
              : 'Permit unconfined plugin'}</button
          ><button
            class="button button-secondary"
            type="button"
            disabled={Boolean(busy)}
            onclick={() => (review = null)}>Cancel</button
          >
        </div>
      </section>
    {/if}
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
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  h3 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
  }
  .muted,
  .facts dt {
    margin: 0;
    color: var(--foreground-muted);
  }
  .row-action {
    text-align: right;
  }
  .review {
    display: grid;
    gap: 0.9rem;
    padding: 1rem;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-control);
  }
  .review p {
    margin: 0;
  }
  .facts {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr));
    gap: 1rem;
    margin: 0;
  }
  .facts dt {
    font-size: var(--text-caption);
  }
  .facts dd {
    margin: 0.25rem 0 0;
    min-width: 0;
  }
  .digest,
  .address {
    overflow-wrap: anywhere;
  }
  .risk {
    padding: 0.9rem 1rem;
    border: 1px solid var(--danger);
    border-radius: var(--radius-control);
    background: var(--danger-soft);
    color: var(--foreground);
  }
  .risk strong {
    color: var(--danger);
  }
  .acknowledge {
    display: flex;
    align-items: start;
    gap: 0.5rem;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 0.65rem;
  }
</style>
