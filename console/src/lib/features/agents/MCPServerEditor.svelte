<script lang="ts">
  import { untrack } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import type { components } from '$lib/management/schema';
  import { useAction } from '$lib/editing/action.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/editing/mutationIntent.svelte';
  import { createServer, saveServer, type MCPServer } from './api';
  import { agentKeys } from './agentKeys';
  import {
    serverPricingForm,
    serverPricingInput,
    type ServerPricingForm
  } from './form';
  let {
    current,
    projects,
    onsaved,
    reload
  }: {
    current: MCPServer | null;
    projects: { id: string; name: string }[];
    reload?: () => void;
    onsaved: (server: MCPServer) => Promise<void>;
  } = $props();
  const initial = untrack(() => current);
  let baseline = $state(initial);
  let name = $state(initial?.name ?? '');
  let project = $state(initial?.project_id ?? '');
  let transport = $state<
    components['schemas']['CreateMCPServerRequest']['transport']
  >(initial?.transport ?? 'streamable_http');
  let endpoint = $state(initial?.endpoint ?? '');
  let enabled = $state(initial?.enabled ?? true);
  let credential = $state('');
  let removeCredential = $state(false);
  let executable = $state(initial?.stdio?.executable ?? '');
  let digest = $state(initial?.stdio?.digest ?? '');
  let oauthEnabled = $state(Boolean(initial?.oauth));
  let clientID = $state(initial?.oauth?.client_id ?? '');
  let audience = $state(initial?.oauth?.audience ?? '');
  let scopes = $state(initial?.oauth?.scopes.join(' ') ?? '');
  let authorizationEndpoint = $state(
    initial?.oauth?.authorization_endpoint ?? ''
  );
  let tokenEndpoint = $state(initial?.oauth?.token_endpoint ?? '');
  let deviceEndpoint = $state(
    initial?.oauth?.device_authorization_endpoint ?? ''
  );
  let entraIssuer = $state(initial?.oauth?.entra_obo?.issuer ?? '');
  let entraSecret = $state('');
  let delegationEnabled = $state(Boolean(initial?.delegation));
  let delegationIssuer = $state(initial?.delegation?.issuer ?? '');
  let delegationAudience = $state(initial?.delegation?.audience ?? '');
  let privateKey = $state('');
  let pricing = $state<ServerPricingForm>(
    initial
      ? serverPricingForm(initial)
      : {
          unitPrice: '',
          toolPrices: {},
          callsPerMinute: '60',
          maxConcurrency: '8',
          perKeyCallsPerMinute: '60',
          perKeyMaxConcurrency: '8'
        }
  );
  const client = useQueryClient();
  const action = useAction(() =>
    client.invalidateQueries({ queryKey: agentKeys.servers() })
  );
  const intent = useMutationIntent();
  const clearSecrets = () => {
    credential = '';
    entraSecret = '';
    privateKey = '';
  };
  usePrincipalLifetime(clearSecrets);
  async function save(event: SubmitEvent) {
    event.preventDefault();
    const prices = serverPricingInput(
      baseline ?? { name, enabled, endpoint, catalog: { tools: [] } },
      pricing
    );
    if (!prices.ok) {
      action.error = prices.message;
      return;
    }
    const body: components['schemas']['CreateMCPServerRequest'] = {
      ...prices.input,
      name,
      enabled,
      project_id: project,
      transport,
      ...(transport === 'stdio'
        ? { endpoint: undefined, stdio: { executable, digest } }
        : {
            endpoint,
            credential: removeCredential ? null : credential || undefined,
            oauth: oauthEnabled
              ? {
                  client_id: clientID,
                  audience,
                  scopes: scopes.split(/\s+/).filter(Boolean),
                  token_endpoint: tokenEndpoint,
                  authorization_endpoint: authorizationEndpoint || undefined,
                  device_authorization_endpoint: deviceEndpoint || undefined,
                  entra_obo: entraIssuer ? { issuer: entraIssuer } : undefined
                }
              : null,
            delegation: delegationEnabled
              ? { issuer: delegationIssuer, audience: delegationAudience }
              : null,
            delegation_private_key: delegationEnabled
              ? privateKey || undefined
              : undefined,
            entra_client_secret:
              oauthEnabled && entraIssuer ? entraSecret || undefined : undefined
          })
    };
    await action.run(
      () =>
        intent.submit({ current: baseline, body }, (input, requestId) => {
          if (!input.current) return createServer(input.body, requestId);
          const {
            project_id: _project,
            transport: _transport,
            stdio: _stdio,
            ...update
          } = input.body;
          void _project;
          void _transport;
          void _stdio;
          return saveServer(input.current, update, requestId);
        }),
      async (server) => {
        baseline = server;
        removeCredential = false;
        clearSecrets();
        await onsaved(server);
      }
    );
  }
</script>

<form class="card" onsubmit={save}>
  <h2>
    {current ? 'Edit and recertify server' : 'Register and certify server'}
  </h2>
  <label>Name<input required maxlength="100" bind:value={name} /></label>
  <label
    >Project<select required disabled={Boolean(current)} bind:value={project}
      ><option value="">Choose project</option
      >{#each projects as item (item.id)}<option value={item.id}
          >{item.name}</option
        >{/each}</select
    ></label
  >
  <label
    >Transport<select disabled={Boolean(current)} bind:value={transport}
      ><option value="streamable_http">Streamable HTTP</option><option
        value="sse">SSE</option
      ><option value="stdio">Local stdio</option></select
    ></label
  >
  {#if transport === 'stdio'}
    <label
      >Approved executable<input
        required
        disabled={Boolean(current)}
        bind:value={executable}
      /></label
    >
    <label
      >Executable SHA256<input
        required
        pattern={'[a-f0-9]{64}'}
        disabled={Boolean(current)}
        bind:value={digest}
      /></label
    >
  {:else}<label
      >Endpoint<input type="url" required bind:value={endpoint} /></label
    >{/if}
  <label><input type="checkbox" bind:checked={enabled} />Enabled</label>
  {#if transport !== 'stdio'}
    <label
      >Bearer credential<input
        type="password"
        autocomplete="new-password"
        bind:value={credential}
        disabled={removeCredential}
      /></label
    >
    {#if baseline?.has_credential}<label
        ><input type="checkbox" bind:checked={removeCredential} />Remove stored
        bearer credential</label
      >{/if}
    <p>Leave secret fields blank to retain existing values</p>
  {/if}
  <fieldset>
    <legend>Pricing and limits</legend>
    <label
      >Price per call<input
        pattern={'[0-9]{1,12}(\\.[0-9]{1,12})?'}
        bind:value={pricing.unitPrice}
      /></label
    >
    <p>Blank means unpriced. Use 0 for free calls</p>
    {#each baseline?.catalog.tools ?? [] as tool (tool.name)}<label
        >{tool.name} price<input
          bind:value={pricing.toolPrices[tool.name]}
        /></label
      >{/each}
    <label
      >Calls per minute<input
        required
        inputmode="numeric"
        bind:value={pricing.callsPerMinute}
      /></label
    >
    <label
      >Concurrent calls<input
        required
        inputmode="numeric"
        bind:value={pricing.maxConcurrency}
      /></label
    >
    <label
      >Calls per minute per key<input
        required
        inputmode="numeric"
        bind:value={pricing.perKeyCallsPerMinute}
      /></label
    >
    <label
      >Concurrent calls per key<input
        required
        inputmode="numeric"
        bind:value={pricing.perKeyMaxConcurrency}
      /></label
    >
  </fieldset>
  {#if transport !== 'stdio'}
    <fieldset>
      <legend>Personal OAuth</legend>
      <label
        ><input type="checkbox" bind:checked={oauthEnabled} />Require individual
        grants</label
      >
      {#if oauthEnabled}
        <label>Client ID<input required bind:value={clientID} /></label>
        <label>Audience<input required bind:value={audience} /></label>
        <label
          >Scopes separated by spaces<input
            required
            bind:value={scopes}
          /></label
        >
        <label
          >Authorization endpoint<input
            type="url"
            bind:value={authorizationEndpoint}
          /></label
        >
        <label
          >Token endpoint<input
            required
            type="url"
            bind:value={tokenEndpoint}
          /></label
        >
        <label
          >Device authorization endpoint<input
            type="url"
            bind:value={deviceEndpoint}
          /></label
        >
        <label>Entra delegated issuer<input bind:value={entraIssuer} /></label>
        <label
          >Entra client secret<input
            type="password"
            autocomplete="new-password"
            bind:value={entraSecret}
          /></label
        >
      {/if}
    </fieldset>
    <fieldset>
      <legend>Delegation</legend>
      <label
        ><input type="checkbox" bind:checked={delegationEnabled} />Enable
        delegation</label
      >
      {#if delegationEnabled}
        <label
          >Issuer<input
            required
            type="url"
            bind:value={delegationIssuer}
          /></label
        >
        <label>Audience<input required bind:value={delegationAudience} /></label
        >
        <label
          >PKCS8 Ed25519 private key<textarea
            autocomplete="off"
            bind:value={privateKey}></textarea></label
        >
      {/if}
    </fieldset>
  {/if}
  <button class="button button-primary" disabled={action.busy}
    >Save and certify</button
  >
  {#if reload && current?.etag !== baseline?.etag}
    <p role="status">
      This resource changed since editing began. Your draft is preserved.
      Reloading discards your edits
    </p>
    <button
      class="button button-secondary"
      type="button"
      disabled={action.busy}
      onclick={reload}>Reload latest</button
    >
  {/if}
  {#if action.error}<p role="alert">{action.error}</p>{/if}
</form>
