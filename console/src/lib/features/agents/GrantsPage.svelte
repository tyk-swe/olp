<script lang="ts">
  import { onMount } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { usePrincipalLifetime } from '$lib/forms/mutationIntent.svelte';
  import {
    claimMCPGrant,
    getServer,
    listMCPGrants,
    listServers,
    revokeMCPGrant,
    saveServer,
    startMCPGrant,
    type MCPServer
  } from './api';

  const access = useRole();
  const client = useQueryClient();
  const grants = createQuery(() => ({
    queryKey: ['mcp-grants'],
    queryFn: listMCPGrants
  }));
  const servers = createQuery(() => ({
    queryKey: ['agent-publications', 'servers'],
    queryFn: listServers,
    enabled: access.allows('GET /api/v1/mcp-servers')
  }));
  const canManage = $derived(
    access.allows('POST /api/v1/mcp-servers/{server_id}/grant-enrollments')
  );
  let selected = $state('');
  let server = $state<MCPServer | null>(null);
  let clientID = $state('');
  let audience = $state('');
  let scopes = $state('');
  let authorizationEndpoint = $state('');
  let tokenEndpoint = $state('');
  let deviceEndpoint = $state('');
  let keyID = $state('');
  let enrollmentID = $state('');
  let handoffToken = $state('');
  let enrollmentURL = $state('');
  let busy = $state(false);
  let message = $state('');
  let error = $state('');
  const captureOwner = usePrincipalLifetime(() => {
    server = null;
    selected = '';
    keyID = '';
    enrollmentID = '';
    handoffToken = '';
    enrollmentURL = '';
    busy = false;
    message = '';
    error = '';
  });

  onMount(() => {
    const [id, token] = window.location.hash.slice(1).split('.');
    if (id && token) {
      enrollmentID = id;
      handoffToken = token;
      window.history.replaceState(
        null,
        '',
        window.location.pathname + window.location.search
      );
    }
  });

  async function loadServer() {
    const owner = captureOwner();
    const requested = selected;
    if (!requested) {
      server = null;
      return;
    }
    try {
      const loaded = await getServer(requested);
      if (!owner() || selected !== requested) return;
      server = loaded;
      clientID = loaded.oauth?.client_id ?? '';
      audience = loaded.oauth?.audience ?? '';
      scopes = loaded.oauth?.scopes.join(' ') ?? '';
      authorizationEndpoint = loaded.oauth?.authorization_endpoint ?? '';
      tokenEndpoint = loaded.oauth?.token_endpoint ?? '';
      deviceEndpoint = loaded.oauth?.device_authorization_endpoint ?? '';
    } catch (cause) {
      if (owner()) error = errorMessage(cause);
    }
  }

  async function saveConfiguration() {
    if (!server || busy) return;
    const owner = captureOwner();
    busy = true;
    error = '';
    message = '';
    try {
      const saved = await saveServer(
        server,
        {
          name: server.name,
          endpoint: server.endpoint,
          enabled: server.enabled,
          oauth: {
            client_id: clientID.trim(),
            audience: audience.trim(),
            scopes: scopes.trim().split(/\s+/),
            authorization_endpoint: authorizationEndpoint.trim(),
            token_endpoint: tokenEndpoint.trim(),
            ...(deviceEndpoint.trim()
              ? { device_authorization_endpoint: deviceEndpoint.trim() }
              : {})
          }
        },
        crypto.randomUUID()
      );
      if (!owner()) return;
      server = saved;
      message = 'Personal authorization configuration saved';
      await client.invalidateQueries({
        queryKey: ['agent-publications', 'servers']
      });
    } catch (cause) {
      if (owner()) error = errorMessage(cause);
    } finally {
      if (owner()) busy = false;
    }
  }

  async function startEnrollment() {
    if (!server || busy) return;
    const owner = captureOwner();
    busy = true;
    error = '';
    enrollmentURL = '';
    try {
      const result = await startMCPGrant(server.id, keyID.trim());
      if (owner()) enrollmentURL = result.enrollment_url;
    } catch (cause) {
      if (owner()) error = errorMessage(cause);
    } finally {
      if (owner()) busy = false;
    }
  }

  async function claimEnrollment() {
    if (!enrollmentID || !handoffToken || busy) return;
    const owner = captureOwner();
    busy = true;
    error = '';
    try {
      await claimMCPGrant(enrollmentID, handoffToken);
      if (!owner()) return;
      handoffToken = '';
      enrollmentID = '';
      message = 'Enrollment claimed. Authorization is pending';
      await client.invalidateQueries({ queryKey: ['mcp-grants'] });
    } catch (cause) {
      if (owner()) error = errorMessage(cause);
    } finally {
      if (owner()) busy = false;
    }
  }

  async function revoke(grantID: string) {
    const owner = captureOwner();
    busy = true;
    error = '';
    try {
      await revokeMCPGrant(grantID);
      if (owner()) await client.invalidateQueries({ queryKey: ['mcp-grants'] });
    } catch (cause) {
      if (owner()) error = errorMessage(cause);
    } finally {
      if (owner()) busy = false;
    }
  }
</script>

<svelte:head><title>Personal tool grants · OpenLLMProxy</title></svelte:head>
<header>
  <p class="eyebrow">Tools</p>
  <h1>Personal tool grants</h1>
  <p>
    Grant standing is bound to one individual and the server's authorization
    settings
  </p>
</header>
{#if error}<p role="alert">{error}</p>{/if}
{#if message}<p role="status">{message}</p>{/if}
{#if handoffToken}
  <section>
    <h2>Claim enrollment</h2>
    <p>This one-use link binds enrollment to your signed-in account</p>
    <button disabled={busy} onclick={claimEnrollment}>Claim enrollment</button>
  </section>
{/if}
{#if canManage}
  <section>
    <h2>Server authorization</h2>
    <label
      >Registered server <select bind:value={selected} onchange={loadServer}>
        <option value="">Select a server</option>
        {#each servers.data ?? [] as item (item.id)}<option value={item.id}
            >{item.name}</option
          >{/each}
      </select></label
    >
    {#if server}
      <form
        onsubmit={(event) => {
          event.preventDefault();
          void saveConfiguration();
        }}
      >
        <label>Client ID <input required bind:value={clientID} /></label>
        <label>Audience <input required bind:value={audience} /></label>
        <label>Scopes <input required bind:value={scopes} /></label>
        <label
          >Authorization endpoint <input
            type="url"
            required
            bind:value={authorizationEndpoint}
          /></label
        >
        <label
          >Token endpoint <input
            type="url"
            required
            bind:value={tokenEndpoint}
          /></label
        >
        <label
          >Device authorization endpoint <input
            type="url"
            bind:value={deviceEndpoint}
          /></label
        >
        <button disabled={busy}>Save authorization settings</button>
      </form>
      <form
        onsubmit={(event) => {
          event.preventDefault();
          void startEnrollment();
        }}
      >
        <label>API key UUID <input required bind:value={keyID} /></label>
        <button disabled={busy || !server.oauth}>Start key enrollment</button>
      </form>
      {#if enrollmentURL}<p>
          <a href={enrollmentURL}>Continue personal enrollment</a>
        </p>{/if}
    {/if}
  </section>
{/if}
<section>
  <h2>Grant standing</h2>
  {#if grants.error}<p role="alert">{errorMessage(grants.error)}</p>
  {:else if grants.isPending}<p>Loading grants</p>
  {:else if !grants.data?.length}<p>No personal grants or enrollments</p>
  {:else}
    <table>
      <thead
        ><tr
          ><th>Server</th><th>Individual</th><th>Audience and scopes</th><th
            >Standing</th
          ><th>Expires</th><th>Action</th></tr
        ></thead
      >
      <tbody
        >{#each grants.data ?? [] as grant (grant.id)}
          <tr>
            <td><code>{grant.binding.server_id}</code></td>
            <td
              >{grant.binding.identity.kind}<br /><code
                >{grant.binding.identity.id}</code
              ></td
            >
            <td
              >{grant.binding.oauth?.audience}<br
              />{grant.binding.oauth?.scopes.join(' ')}</td
            >
            <td>{grant.state}</td><td
              >{new Date(grant.expires_at).toLocaleString()}</td
            >
            <td
              >{#if grant.grant_id && grant.state !== 'revoked'}<button
                  disabled={busy}
                  onclick={() => revoke(grant.grant_id!)}>Revoke</button
                >{/if}</td
            >
          </tr>
        {/each}</tbody
      >
    </table>
  {/if}
</section>
