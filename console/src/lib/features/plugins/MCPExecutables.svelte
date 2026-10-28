<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
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

  let { enabled, canManage }: { enabled: boolean; canManage: boolean } =
    $props();
  const files = createQuery(() => ({
    queryKey: ['mcp-executables'],
    queryFn: ({ signal }) => listMCPExecutables(signal),
    enabled: enabled && canManage
  }));
  let review = $state<MCPExecutablePermission | null>(null);
  let acknowledged = $state(false);
  let busy = $state(false);
  let problem = $state('');
  let reauthenticating = $state(false);
  let singleSignOn = $state(false);

  async function inspect(name: string) {
    busy = true;
    problem = '';
    acknowledged = false;
    try {
      review = await reviewMCPExecutable(name);
    } catch (cause) {
      problem = errorMessage(cause);
    } finally {
      busy = false;
    }
  }

  async function identity() {
    const identities = await listOidcIdentities();
    singleSignOn = identities.oidc_reauthentication_available === true;
    if (identities.has_local_password) {
      reauthenticating = true;
      return;
    }
    window.location.assign(
      await beginIdentityReauthentication('plugin_permit')
    );
  }

  async function permit() {
    if (!review || !acknowledged) return;
    busy = true;
    problem = '';
    try {
      review = await permitMCPExecutable(review);
      acknowledged = false;
    } catch (cause) {
      if (needsReauthentication(cause)) {
        try {
          await identity();
        } catch (failure) {
          problem = errorMessage(failure);
        }
      } else problem = errorMessage(cause);
    } finally {
      busy = false;
    }
  }

  async function confirm(password: string) {
    busy = true;
    problem = '';
    try {
      await reauthenticateWithPassword(password, 'plugin_permit');
      reauthenticating = false;
      await permit();
    } catch (cause) {
      problem = errorMessage(cause);
    } finally {
      busy = false;
    }
  }

  async function singleSignOnIdentity() {
    busy = true;
    try {
      window.location.assign(
        await beginIdentityReauthentication('plugin_permit')
      );
    } catch (cause) {
      problem = errorMessage(cause);
    } finally {
      busy = false;
    }
  }

  async function revoke() {
    if (!review) return;
    busy = true;
    problem = '';
    try {
      await revokeMCPExecutable(review);
      review = await reviewMCPExecutable(review.name);
    } catch (cause) {
      problem = errorMessage(cause);
    } finally {
      busy = false;
    }
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
      {#if problem}<div role="alert" class="inline-problem">{problem}</div>{/if}
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
                      disabled={busy}
                      onclick={() => inspect(file.name)}
                      >Review MCP executable</button
                    ></td
                  ></tr
                >{/each}
            </tbody>
          </table>
        </div>
      {/if}
      {#if review}
        <section class="review">
          <h3>Review {review.name}</h3>
          <p>Metadata review does not start the executable</p>
          <p><code>{review.digest}</code></p>
          {#if review.permitted}<p role="status">
              This digest is permitted for MCP execution
            </p>
            <button
              class="button button-secondary"
              disabled={busy}
              onclick={revoke}>Revoke MCP permission</button
            >
          {:else}
            <label
              ><input type="checkbox" bind:checked={acknowledged} /> I understand
              this executable runs unconfined with the gateway's privileges</label
            >
            <p class="muted">
              Permission requires recent authentication. After single sign-on,
              review this executable again and choose Permit
            </p>
            <button
              class="button button-primary"
              disabled={busy || !acknowledged}
              onclick={permit}>Permit MCP executable</button
            >
          {/if}
        </section>
      {/if}
    {/if}
  {/if}
</section>

{#if reauthenticating}<ReauthenticateDialog
    title="Permit native MCP execution"
    description="Confirm your identity to permit this exact executable digest"
    {busy}
    error={problem}
    onOidc={singleSignOn ? singleSignOnIdentity : undefined}
    onConfirm={confirm}
    onCancel={() => (reauthenticating = false)}
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
