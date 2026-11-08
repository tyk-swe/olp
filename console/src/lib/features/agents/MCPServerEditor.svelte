<script lang="ts">
  import { untrack } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import type { components } from '#lib/management/schema.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { guardUnsavedChanges } from '#lib/editing/unsavedChanges.js';
  import ConflictNotice from '#lib/shell/ConflictNotice.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
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
    const update = {
      ...prices.input,
      name,
      enabled,
      ...(transport === 'stdio'
        ? { endpoint: undefined }
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
    const create: components['schemas']['CreateMCPServerRequest'] = {
      ...update,
      project_id: project,
      transport,
      ...(transport === 'stdio' ? { stdio: { executable, digest } } : {})
    };
    await action.run(
      () =>
        intent.submit(
          { current: baseline, create, update },
          (input, requestId) =>
            input.current
              ? saveServer(input.current, input.update, requestId)
              : createServer(input.create, requestId)
        ),
      async (server) => {
        baseline = server;
        removeCredential = false;
        clearSecrets();
        clean = draft();
        await onsaved(server);
      }
    );
  }
  const draft = () =>
    JSON.stringify([
      name,
      project,
      transport,
      endpoint,
      enabled,
      credential,
      removeCredential,
      executable,
      digest,
      oauthEnabled,
      clientID,
      audience,
      scopes,
      authorizationEndpoint,
      tokenEndpoint,
      deviceEndpoint,
      entraIssuer,
      entraSecret,
      delegationEnabled,
      delegationIssuer,
      delegationAudience,
      privateKey,
      pricing
    ]);
  let clean = $state(draft());
  export function isDirty() {
    return draft() !== clean;
  }
  guardUnsavedChanges(isDirty);
</script>

<form class="form-card" onsubmit={save}>
  <label class="form-field"
    >Name<input required maxlength="100" bind:value={name} /></label
  >
  <label class="form-field"
    >Project<select required disabled={Boolean(current)} bind:value={project}
      ><option value="">Choose project</option
      >{#each projects as item (item.id)}<option value={item.id}
          >{item.name}</option
        >{/each}</select
    ></label
  >
  <label class="form-field"
    >Transport<select disabled={Boolean(current)} bind:value={transport}
      ><option value="streamable_http">Streamable HTTP</option><option
        value="sse">SSE</option
      ><option value="stdio">Local stdio</option></select
    ></label
  >
  {#if transport === 'stdio'}
    <label class="form-field"
      >Approved executable<input
        required
        disabled={Boolean(current)}
        bind:value={executable}
      /></label
    >
    <label class="form-field"
      >Executable SHA256<input
        required
        pattern={'[a-f0-9]{64}'}
        disabled={Boolean(current)}
        bind:value={digest}
      /></label
    >
  {:else}<label class="form-field"
      >Endpoint<input type="url" required bind:value={endpoint} /></label
    >{/if}
  <label class="check-field"
    ><input type="checkbox" bind:checked={enabled} />Enabled</label
  >
  {#if transport !== 'stdio'}
    <label class="form-field"
      >Bearer credential<input
        type="password"
        autocomplete="new-password"
        bind:value={credential}
        disabled={removeCredential}
      /></label
    >
    {#if baseline?.has_credential}<label class="check-field"
        ><input type="checkbox" bind:checked={removeCredential} />Remove stored
        bearer credential</label
      >{/if}
    <p class="inline-status">
      Leave secret fields blank to retain existing values
    </p>
  {/if}
  <fieldset>
    <legend>Pricing and limits</legend>
    <label class="form-field"
      >Price per call<input
        pattern={'[0-9]{1,12}(\\.[0-9]{1,12})?'}
        bind:value={pricing.unitPrice}
      /></label
    >
    <p class="inline-status">Blank means unpriced. Use 0 for free calls</p>
    {#each baseline?.catalog.tools ?? [] as tool (tool.name)}<label
        class="form-field"
        >{tool.name} price<input
          bind:value={pricing.toolPrices[tool.name]}
        /></label
      >{/each}
    <label class="form-field"
      >Calls per minute<input
        required
        inputmode="numeric"
        bind:value={pricing.callsPerMinute}
      /></label
    >
    <label class="form-field"
      >Concurrent calls<input
        required
        inputmode="numeric"
        bind:value={pricing.maxConcurrency}
      /></label
    >
    <label class="form-field"
      >Calls per minute per key<input
        required
        inputmode="numeric"
        bind:value={pricing.perKeyCallsPerMinute}
      /></label
    >
    <label class="form-field"
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
      <label class="check-field"
        ><input type="checkbox" bind:checked={oauthEnabled} />Require individual
        grants</label
      >
      {#if oauthEnabled}
        <label class="form-field"
          >Client ID<input required bind:value={clientID} /></label
        >
        <label class="form-field"
          >Audience<input required bind:value={audience} /></label
        >
        <label class="form-field"
          >Scopes separated by spaces<input
            required
            bind:value={scopes}
          /></label
        >
        <label class="form-field"
          >Authorization endpoint<input
            type="url"
            bind:value={authorizationEndpoint}
          /></label
        >
        <label class="form-field"
          >Token endpoint<input
            required
            type="url"
            bind:value={tokenEndpoint}
          /></label
        >
        <label class="form-field"
          >Device authorization endpoint<input
            type="url"
            bind:value={deviceEndpoint}
          /></label
        >
        <label class="form-field"
          >Entra delegated issuer<input bind:value={entraIssuer} /></label
        >
        <label class="form-field"
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
      <label class="check-field"
        ><input type="checkbox" bind:checked={delegationEnabled} />Enable
        delegation</label
      >
      {#if delegationEnabled}
        <label class="form-field"
          >Issuer<input
            required
            type="url"
            bind:value={delegationIssuer}
          /></label
        >
        <label class="form-field"
          >Audience<input required bind:value={delegationAudience} /></label
        >
        <label class="form-field full"
          >PKCS8 Ed25519 private key<textarea
            autocomplete="off"
            bind:value={privateKey}></textarea></label
        >
      {/if}
    </fieldset>
  {/if}
  <div class="page-actions">
    <button class="button button-primary" disabled={action.busy}
      >Save and certify</button
    >
  </div>
  {#if reload && current?.etag !== baseline?.etag}
    <ConflictNotice notice="newer" onReload={reload} disabled={action.busy} />
  {/if}
  {#if action.error}<p class="inline-problem" role="alert">
      {action.error}
    </p>{/if}
</form>
