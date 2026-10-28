<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { pluginKeys } from './pluginKeys';
  import { errorMessage } from '$lib/api/http';
  import { formatBytes } from '$lib/format';
  import ReauthenticateDialog from '$lib/components/ReauthenticateDialog.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import {
    beginIdentityReauthentication,
    listOidcIdentities,
    reauthenticateWithPassword
  } from '$lib/features/access/profile/api';
  import {
    listMCPExecutables,
    reviewMCPExecutable,
    permitMCPExecutable,
    revokeMCPExecutable,
    needsReauthentication,
    shortDigest,
    type MCPExecutablePermission
  } from './api';

  const { enabled, canManage }: { enabled: boolean; canManage: boolean } =
    $props();
  const files = createQuery(() => ({
    queryKey: pluginKeys.mcpExecutables(),
    queryFn: ({ signal }) => listMCPExecutables(signal),
    enabled: enabled && canManage
  }));
  type State = Readonly<{
    review: MCPExecutablePermission | null;
    acknowledged: boolean;
    busy: boolean;
    problem: string;
    refreshProblem: string;
    notice: string;
    reauthenticating: boolean;
    singleSignOn: boolean;
  }>;
  const state = writable<State>({
    review: null,
    acknowledged: false,
    busy: false,
    problem: '',
    refreshProblem: '',
    notice: '',
    reauthenticating: false,
    singleSignOn: false
  });
  function patch(update: Partial<State>) {
    state.update((current) => ({ ...current, ...update }));
  }

  async function inspect(name: string) {
    patch({
      busy: true,
      problem: '',
      refreshProblem: '',
      notice: '',
      acknowledged: false
    });
    try {
      patch({ review: await reviewMCPExecutable(name) });
    } catch (cause) {
      patch({ problem: errorMessage(cause) });
    } finally {
      patch({ busy: false });
    }
  }

  async function identity() {
    const identities = await listOidcIdentities();
    patch({
      singleSignOn: identities.oidc_reauthentication_available === true
    });
    if (identities.has_local_password) {
      patch({ reauthenticating: true });
      return;
    }
    window.location.assign(
      await beginIdentityReauthentication('plugin_permit')
    );
  }

  async function permit() {
    const selected = $state.review;
    if (!selected || !$state.acknowledged) return;
    patch({ busy: true, problem: '' });
    try {
      patch({
        review: await permitMCPExecutable(selected),
        acknowledged: false,
        notice: ''
      });
    } catch (cause) {
      if (needsReauthentication(cause)) {
        try {
          await identity();
        } catch (failure) {
          patch({ problem: errorMessage(failure) });
        }
      } else patch({ problem: errorMessage(cause) });
    } finally {
      patch({ busy: false });
    }
  }

  async function confirm(password: string) {
    patch({ busy: true, problem: '' });
    try {
      await reauthenticateWithPassword(password, 'plugin_permit');
      patch({ reauthenticating: false });
      await permit();
    } catch (cause) {
      patch({ problem: errorMessage(cause) });
    } finally {
      patch({ busy: false });
    }
  }

  async function singleSignOnIdentity() {
    patch({ busy: true });
    try {
      window.location.assign(
        await beginIdentityReauthentication('plugin_permit')
      );
    } catch (cause) {
      patch({ problem: errorMessage(cause) });
    } finally {
      patch({ busy: false });
    }
  }

  async function refreshReview(name: string) {
    patch({ busy: true, refreshProblem: '' });
    try {
      patch({ review: await reviewMCPExecutable(name) });
    } catch (cause) {
      patch({ refreshProblem: errorMessage(cause) });
    } finally {
      patch({ busy: false });
    }
  }

  async function revoke() {
    const selected = $state.review;
    if (!selected) return;
    patch({ busy: true, problem: '', refreshProblem: '' });
    try {
      await revokeMCPExecutable(selected);
    } catch (cause) {
      patch({ problem: errorMessage(cause), busy: false });
      return;
    }
    patch({
      review: { ...selected, permitted: false },
      notice: 'MCP permission was revoked',
      acknowledged: false
    });
    await refreshReview(selected.name);
  }
</script>

<section class="card native-mcp" aria-labelledby="mcp-executables-heading">
  <header>
    <p class="eyebrow">Experimental</p>
    <h2 id="mcp-executables-heading">Unconfined MCP executables</h2>
  </header>
  {#if !enabled}
    <p class="muted">
      The deployment must set <code>OLP_UNCONFINED_PLUGIN_DIR</code> before installed
      MCP executables are available
    </p>
  {:else}
    <p class="muted">
      These processes run with the gateway's operating system privileges and can
      access its files and network. Review and permit only a build you trust.
      Registration alone does not permit execution
    </p>
    {#if !canManage}<ReadOnlyNote
        >Only an owner can review, permit or revoke executable digests</ReadOnlyNote
      >
    {:else}
      {#if $state.problem}<div role="alert" class="inline-problem">
          {$state.problem}
        </div>{/if}
      {#if $state.notice}<p role="status">{$state.notice}</p>{/if}
      {#if $state.refreshProblem}<div role="alert">
          Status refresh failed. {$state.refreshProblem}<button
            class="text-button"
            disabled={$state.busy}
            onclick={() => $state.review && refreshReview($state.review.name)}
            >Retry status refresh</button
          >
        </div>{/if}
      {#if files.isPending}<p role="status">Loading installed executables</p>
      {:else if files.isError}<div role="alert" class="inline-problem">
          {errorMessage(files.error)}
          <button class="text-button" onclick={() => files.refetch()}
            >Retry</button
          >
        </div>
      {:else if !files.data?.length}<p class="muted">
          No executables are installed in the deployment directory
        </p>
      {:else}
        <div class="table-shell">
          <table class="data-table">
            <thead
              ><tr
                ><th>Executable</th><th>SHA-256</th><th>Size</th><th>Action</th
                ></tr
              ></thead
            ><tbody>
              {#each files.data as file (file.name)}<tr
                  ><td><code>{file.name}</code></td><td
                    ><code title={file.digest}>{shortDigest(file.digest)}</code
                    ></td
                  ><td>{formatBytes(file.size_bytes)}</td><td
                    ><button
                      class="button button-secondary"
                      disabled={$state.busy}
                      onclick={() => inspect(file.name)}
                      >Review MCP executable</button
                    ></td
                  ></tr
                >{/each}
            </tbody>
          </table>
        </div>
      {/if}
      {#if $state.review}
        <section class="review">
          <h3>Review {$state.review.name}</h3>
          <p>Metadata review does not start the executable</p>
          <p><code>{$state.review.digest}</code></p>
          {#if $state.review.permitted}<p role="status">
              This digest is permitted for MCP execution
            </p>
            <button
              class="button button-secondary"
              disabled={$state.busy}
              onclick={revoke}>Revoke MCP permission</button
            >
          {:else}
            <label
              ><input
                type="checkbox"
                checked={$state.acknowledged}
                onchange={(event) =>
                  patch({ acknowledged: event.currentTarget.checked })}
              /> I understand this executable runs unconfined with the gateway's privileges</label
            >
            <p class="muted">
              Permission requires recent authentication. After single sign-on,
              review this executable again and choose Permit
            </p>
            <button
              class="button button-primary"
              disabled={$state.busy || !$state.acknowledged}
              onclick={permit}>Permit MCP executable</button
            >
          {/if}
        </section>
      {/if}
    {/if}
  {/if}
</section>

{#if $state.reauthenticating}<ReauthenticateDialog
    title="Permit native MCP execution"
    description="Confirm your identity to permit this exact executable digest"
    busy={$state.busy}
    error={$state.problem}
    onOidc={$state.singleSignOn ? singleSignOnIdentity : undefined}
    onConfirm={confirm}
    onCancel={() => patch({ reauthenticating: false })}
  />{/if}

<style>
  .native-mcp {
    margin-top: 1rem;
    padding: 1.5rem;
    display: grid;
    gap: 1rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
  }
  .review {
    display: grid;
    gap: 0.75rem;
    min-width: 0;
  }
  code {
    overflow-wrap: anywhere;
  }
</style>
