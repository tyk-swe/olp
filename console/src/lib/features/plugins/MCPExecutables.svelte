<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { pluginKeys } from './pluginKeys';
  import { errorMessage } from '#lib/management/http.js';
  import { formatBytes } from '#lib/format.js';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import {
    beginIdentityReauthentication,
    listOidcIdentities,
    ReauthenticateDialog,
    reauthenticateWithPassword
  } from '#lib/features/identity/index.js';
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
    problem: string;
    refreshProblem: string;
    notice: string;
    reauthenticating: boolean;
    singleSignOn: boolean;
  }>;
  const panel = writable<State>({
    review: null,
    acknowledged: false,
    problem: '',
    refreshProblem: '',
    notice: '',
    reauthenticating: false,
    singleSignOn: false
  });
  function patch(update: Partial<State>) {
    panel.update((current) => ({ ...current, ...update }));
  }

  const mcpAction = useAction();
  let readBusy = $state(false);
  const busy = $derived(readBusy || mcpAction.busy);

  async function inspect(name: string) {
    patch({
      problem: '',
      refreshProblem: '',
      notice: '',
      acknowledged: false
    });
    readBusy = true;
    const result = await readResult(() => reviewMCPExecutable(name));
    readBusy = false;
    if (result.ok) patch({ review: result.value });
    else patch({ problem: errorMessage(result.error) });
  }

  async function identity() {
    await mcpAction.run(
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
          patch({
            singleSignOn:
              result.identities.oidc_reauthentication_available === true,
            reauthenticating: true
          });
          return;
        }
        window.location.assign(result.redirect);
      },
      { key: 'identity', failure: (failure) => errorMessage(failure) }
    );
    patch({ problem: mcpAction.error });
  }

  async function permit() {
    const selected = $panel.review;
    if (!selected || !$panel.acknowledged) return;
    patch({ problem: '' });
    let failureCause: unknown;
    const committed = await mcpAction.run(
      () => permitMCPExecutable(selected),
      (review, current) => {
        if (!current()) return;
        patch({ review, acknowledged: false, notice: '' });
      },
      {
        key: 'permit',
        failure: (cause) => {
          failureCause = cause;
          return errorMessage(cause);
        }
      }
    );
    if (!committed && needsReauthentication(failureCause)) {
      await identity();
      return;
    }
    patch({ problem: mcpAction.error });
  }

  async function confirm(password: string) {
    patch({ problem: '' });
    const ok = await mcpAction.run(
      () => reauthenticateWithPassword(password, 'plugin_permit'),
      undefined,
      { key: 'confirm', failure: (cause) => errorMessage(cause) }
    );
    if (!ok) {
      patch({ problem: mcpAction.error });
      return;
    }
    patch({ reauthenticating: false });
    await permit();
  }

  async function singleSignOnIdentity() {
    await mcpAction.run(
      async () => {
        window.location.assign(
          await beginIdentityReauthentication('plugin_permit')
        );
      },
      undefined,
      { key: 'sso', failure: (cause) => errorMessage(cause) }
    );
    patch({ problem: mcpAction.error });
  }

  async function refreshReview(name: string) {
    patch({ refreshProblem: '' });
    readBusy = true;
    const result = await readResult(() => reviewMCPExecutable(name));
    readBusy = false;
    if (result.ok) patch({ review: result.value });
    else patch({ refreshProblem: errorMessage(result.error) });
  }

  async function revoke() {
    const selected = $panel.review;
    if (!selected) return;
    patch({ problem: '', refreshProblem: '' });
    const ok = await mcpAction.run(
      () => revokeMCPExecutable(selected),
      async (_, current) => {
        if (!current()) return;
        patch({
          review: { ...selected, permitted: false },
          notice: 'MCP permission was revoked',
          acknowledged: false
        });
      },
      { key: 'revoke', failure: (cause) => errorMessage(cause) }
    );
    if (!ok) {
      patch({ problem: mcpAction.error });
      return;
    }
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
      {#if $panel.problem}<div role="alert" class="inline-problem">
          {$panel.problem}
        </div>{/if}
      {#if $panel.notice}<p role="status">{$panel.notice}</p>{/if}
      {#if $panel.refreshProblem}<div role="alert">
          Status refresh failed. {$panel.refreshProblem}<button
            class="text-button"
            disabled={busy}
            onclick={() => $panel.review && refreshReview($panel.review.name)}
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
        <ScrollTable label="MCP executables">
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
                      disabled={busy}
                      onclick={() => inspect(file.name)}
                      >Review MCP executable</button
                    ></td
                  ></tr
                >{/each}
            </tbody>
          </table>
        </ScrollTable>
      {/if}
      {#if $panel.review}
        <section class="review">
          <h3>Review {$panel.review.name}</h3>
          <p>Metadata review does not start the executable</p>
          <p><code>{$panel.review.digest}</code></p>
          {#if $panel.review.permitted}<p role="status">
              This digest is permitted for MCP execution
            </p>
            <button
              class="button button-secondary"
              disabled={busy}
              onclick={revoke}>Revoke MCP permission</button
            >
          {:else}
            <label
              ><input
                type="checkbox"
                checked={$panel.acknowledged}
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
              disabled={busy || !$panel.acknowledged}
              onclick={permit}>Permit MCP executable</button
            >
          {/if}
        </section>
      {/if}
    {/if}
  {/if}
</section>

{#if $panel.reauthenticating}<ReauthenticateDialog
    title="Permit native MCP execution"
    description="Confirm your identity to permit this exact executable digest"
    {busy}
    error={$panel.problem}
    onOidc={$panel.singleSignOn ? singleSignOnIdentity : undefined}
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
    font-size: var(--text-body);
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
