<script lang="ts">
  import { onMount } from 'svelte';
  import { writable } from 'svelte/store';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/forms/mutationIntent.svelte';
  import { agentKeys } from './agentKeys';
  import {
    claimMCPGrant,
    authorizeMCPGrant,
    completeMCPGrant,
    pollMCPGrant,
    type MCPGrantAuthorization,
    getServer,
    listMCPGrants,
    listServers,
    revokeMCPGrant,
    saveServer,
    startMCPGrant,
    type MCPServer
  } from './api';

  type Fields = {
    delegatedIssuer: string;
    delegatedAudience: string;
    signingKey: string;
    entraIssuer: string;
    entraClientSecret: string;
    clientID: string;
    audience: string;
    scopes: string;
    authorizationEndpoint: string;
    tokenEndpoint: string;
    deviceEndpoint: string;
    keyID: string;
  };
  type PageState = {
    selection: { readonly id: string };
    server: MCPServer | null;
    fields: Fields;
    enrollmentID: string;
    handoffToken: string;
    callbackCode: string;
    callbackState: string;
    authorization: MCPGrantAuthorization | undefined;
    enrollmentURL: string;
    busy: boolean;
    message: string;
    error: string;
  };
  const initialState = (): PageState => ({
    selection: { id: '' },
    server: null,
    fields: {
      delegatedIssuer: '',
      delegatedAudience: '',
      signingKey: '',
      entraIssuer: '',
      entraClientSecret: '',
      clientID: '',
      audience: '',
      scopes: '',
      authorizationEndpoint: '',
      tokenEndpoint: '',
      deviceEndpoint: '',
      keyID: ''
    },
    enrollmentID: '',
    handoffToken: '',
    callbackCode: '',
    callbackState: '',
    authorization: undefined,
    enrollmentURL: '',
    busy: false,
    message: '',
    error: ''
  });
  const state = writable(initialState());
  const access = useRole();
  const client = useQueryClient();
  const intent = useMutationIntent();
  const grants = createQuery(() => ({
    queryKey: agentKeys.grants(),
    queryFn: listMCPGrants
  }));
  const servers = createQuery(() => ({
    queryKey: agentKeys.servers(),
    queryFn: listServers,
    enabled: access.allows('GET /api/v1/mcp-servers')
  }));
  const canManage = $derived(
    access.allows('POST /api/v1/mcp-servers/{server_id}/grant-enrollments')
  );
  const captureOwner = usePrincipalLifetime(() => state.set(initialState()));
  const patch = (values: Partial<PageState>) =>
    state.update((current) => ({ ...current, ...values }));
  const setField = (field: keyof Fields, value: string) =>
    state.update((current) => ({
      ...current,
      fields: { ...current.fields, [field]: value }
    }));

  function consumeEnrollment(fragment: string) {
    const [id, token] = fragment.split('.');
    if (!id || !token) return;
    patch({
      enrollmentID: id,
      handoffToken: token,
      callbackCode: '',
      callbackState: '',
      authorization: undefined,
      enrollmentURL: '',
      error: '',
      message: '',
      busy: false
    });
    window.history.replaceState(
      null,
      '',
      window.location.pathname + window.location.search
    );
  }
  onMount(() => {
    const callback = new URL(window.location.href);
    const code = callback.searchParams.get('code');
    const callbackState = callback.searchParams.get('state');
    if (code && callbackState) {
      patch({
        enrollmentID: callbackState.split('.')[0] ?? '',
        callbackCode: code,
        callbackState
      });
      window.history.replaceState(null, '', window.location.pathname);
      return;
    }
    consumeEnrollment(window.location.hash.slice(1));
  });

  async function loadServer(requested: string) {
    const owner = captureOwner();
    const selection = { id: requested };
    intent.reset();
    state.set({ ...initialState(), selection });
    if (!requested) return;
    try {
      const loaded = await getServer(requested);
      if (!owner() || $state.selection !== selection) return;
      patch({
        server: loaded,
        fields: {
          delegatedIssuer: loaded.delegation?.issuer ?? '',
          delegatedAudience: loaded.delegation?.audience ?? '',
          signingKey: '',
          entraIssuer: loaded.oauth?.entra_obo?.issuer ?? '',
          entraClientSecret: '',
          clientID: loaded.oauth?.client_id ?? '',
          audience: loaded.oauth?.audience ?? '',
          scopes: loaded.oauth?.scopes.join(' ') ?? '',
          authorizationEndpoint: loaded.oauth?.authorization_endpoint ?? '',
          tokenEndpoint: loaded.oauth?.token_endpoint ?? '',
          deviceEndpoint: loaded.oauth?.device_authorization_endpoint ?? '',
          keyID: ''
        }
      });
    } catch (cause) {
      if (owner() && $state.selection === selection)
        patch({ error: errorMessage(cause) });
    }
  }

  async function refresh(
    queryKey:
      | ReturnType<typeof agentKeys.grants>
      | ReturnType<typeof agentKeys.servers>,
    active: () => boolean
  ) {
    try {
      await client.invalidateQueries({ queryKey }, { throwOnError: true });
    } catch (cause) {
      if (active())
        patch({
          error: `Write saved, but refreshing failed. ${errorMessage(cause)}`
        });
    }
  }

  async function saveConfiguration() {
    const observed = $state;
    if (!observed.server || observed.busy) return;
    const owner = captureOwner();
    const active = () => owner() && $state.selection === observed.selection;
    const fields = observed.fields;
    patch({ busy: true, error: '', message: '' });
    try {
      const saved = await intent.submit(
        {
          current: observed.server,
          input: {
            name: observed.server.name,
            endpoint: observed.server.endpoint,
            enabled: observed.server.enabled,
            delegation: fields.delegatedIssuer.trim()
              ? {
                  issuer: fields.delegatedIssuer.trim(),
                  audience: fields.delegatedAudience.trim()
                }
              : null,
            ...(fields.signingKey
              ? { delegation_private_key: fields.signingKey }
              : {}),
            ...(fields.entraClientSecret
              ? { entra_client_secret: fields.entraClientSecret }
              : {}),
            oauth: {
              client_id: fields.clientID.trim(),
              audience: fields.audience.trim(),
              scopes: fields.scopes.trim().split(/\s+/),
              ...(fields.entraIssuer.trim()
                ? { entra_obo: { issuer: fields.entraIssuer.trim() } }
                : {
                    authorization_endpoint: fields.authorizationEndpoint.trim()
                  }),
              token_endpoint: fields.tokenEndpoint.trim(),
              ...(!fields.entraIssuer.trim() && fields.deviceEndpoint.trim()
                ? {
                    device_authorization_endpoint: fields.deviceEndpoint.trim()
                  }
                : {})
            }
          }
        },
        (submission, requestId) =>
          saveServer(submission.current, submission.input, requestId)
      );
      if (!active()) return;
      patch({
        server: saved,
        fields: {
          ...$state.fields,
          entraClientSecret:
            $state.fields.entraClientSecret === fields.entraClientSecret
              ? ''
              : $state.fields.entraClientSecret,
          signingKey:
            $state.fields.signingKey === fields.signingKey
              ? ''
              : $state.fields.signingKey
        },
        message: 'Personal authorization configuration saved'
      });
      await refresh(agentKeys.servers(), active);
    } catch (cause) {
      if (active()) patch({ error: errorMessage(cause) });
    } finally {
      if (active()) patch({ busy: false });
    }
  }

  async function startEnrollment() {
    const observed = $state;
    if (!observed.server || observed.busy) return;
    const owner = captureOwner();
    const active = () =>
      owner() &&
      $state.selection === observed.selection &&
      $state.enrollmentID === observed.enrollmentID;
    patch({ busy: true, error: '', enrollmentURL: '' });
    try {
      const result = await startMCPGrant(
        observed.server.id,
        observed.fields.keyID.trim()
      );
      if (active()) patch({ enrollmentURL: result.enrollment_url });
    } catch (cause) {
      if (active()) patch({ error: errorMessage(cause) });
    } finally {
      if (active()) patch({ busy: false });
    }
  }

  async function claimEnrollment() {
    const observed = $state;
    if (!observed.enrollmentID || !observed.handoffToken || observed.busy)
      return;
    const owner = captureOwner();
    const active = () =>
      owner() &&
      $state.selection === observed.selection &&
      $state.enrollmentID === observed.enrollmentID;
    patch({ busy: true, error: '' });
    try {
      const claimed = await claimMCPGrant(
        observed.enrollmentID,
        observed.handoffToken
      );
      if (!active() || $state.handoffToken !== observed.handoffToken) return;
      patch({
        handoffToken: '',
        enrollmentID: claimed.enrollment.id,
        message: 'Enrollment claimed. Authorization is pending'
      });
      await refresh(agentKeys.grants(), active);
    } catch (cause) {
      if (active() && $state.handoffToken === observed.handoffToken)
        patch({ error: errorMessage(cause) });
    } finally {
      if (active()) patch({ busy: false });
    }
  }

  async function authorize(flow: 'authorization_code' | 'device_code') {
    const observed = $state;
    if (observed.busy) return;
    const owner = captureOwner();
    const active = () =>
      owner() &&
      $state.selection === observed.selection &&
      $state.enrollmentID === observed.enrollmentID;
    patch({ busy: true, error: '', message: '' });
    try {
      const authorization = await authorizeMCPGrant(
        observed.enrollmentID,
        flow
      );
      if (active()) patch({ authorization });
    } catch (cause) {
      if (active()) patch({ error: errorMessage(cause) });
    } finally {
      if (active()) patch({ busy: false });
    }
  }

  async function finish(poll: boolean) {
    const observed = $state;
    if (observed.busy) return;
    const owner = captureOwner();
    const active = () =>
      owner() &&
      $state.selection === observed.selection &&
      $state.enrollmentID === observed.enrollmentID;
    patch({ busy: true, error: '', message: '' });
    try {
      const authorization = poll
        ? await pollMCPGrant(observed.enrollmentID)
        : await completeMCPGrant(
            observed.enrollmentID,
            observed.callbackCode,
            observed.callbackState
          );
      if (!active()) return;
      patch({
        authorization: { ...observed.authorization, ...authorization },
        callbackCode: '',
        callbackState: '',
        message:
          authorization.state === 'active'
            ? 'Personal authorization is active'
            : `Personal authorization is ${authorization.state}`
      });
      await refresh(agentKeys.grants(), active);
    } catch (cause) {
      if (active()) patch({ error: errorMessage(cause) });
    } finally {
      if (active()) patch({ busy: false });
    }
  }

  async function revoke(grantID: string) {
    const owner = captureOwner();
    patch({ busy: true, error: '' });
    try {
      await revokeMCPGrant(grantID);
      if (owner()) await refresh(agentKeys.grants(), owner);
    } catch (cause) {
      if (owner()) patch({ error: errorMessage(cause) });
    } finally {
      if (owner()) patch({ busy: false });
    }
  }
</script>

<svelte:window
  onhashchange={() => consumeEnrollment(window.location.hash.slice(1))}
/>

<svelte:head><title>Personal tool grants · OpenLLMProxy</title></svelte:head>
<header>
  <p class="eyebrow">Tools</p>
  <h1>Personal tool grants</h1>
  <p>
    Grant standing is bound to one individual and the server's authorization
    settings
  </p>
</header>
{#if $state.error}<p role="alert">{$state.error}</p>{/if}
{#if $state.message}<p role="status">{$state.message}</p>{/if}
{#if $state.handoffToken}
  <section>
    <h2>Claim enrollment</h2>
    <p>This one-use link binds enrollment to your signed-in account</p>
    <button disabled={$state.busy} onclick={claimEnrollment}
      >Claim enrollment</button
    >
  </section>
{/if}
{#if $state.enrollmentID && !$state.handoffToken && $state.authorization?.state !== 'active'}
  <section>
    <h2>Authorize personal access</h2>
    {#if $state.callbackCode}
      <button disabled={$state.busy} onclick={() => finish(false)}
        >Complete authorization</button
      >
    {:else if $state.authorization?.device}
      <p>
        Enter {$state.authorization.device.user_code} at
        <a
          href={$state.authorization.device.verification_url}
          target="_blank"
          rel="noreferrer">the authorization page</a
        >
      </p>
      <p>Check again after {$state.authorization.device.interval} seconds</p>
      <button disabled={$state.busy} onclick={() => finish(true)}
        >Check device approval</button
      >
    {:else if $state.authorization?.authorization_url}
      <a href={$state.authorization.authorization_url}>Sign in upstream</a>
    {:else}
      <button
        disabled={$state.busy}
        onclick={() => authorize('authorization_code')}
        >Authorize in browser</button
      >
      <button disabled={$state.busy} onclick={() => authorize('device_code')}
        >Use device authorization</button
      >
    {/if}
  </section>
{/if}

{#if canManage}
  <section>
    <h2>Server authorization</h2>
    <label
      >Registered server <select
        value={$state.selection.id}
        onchange={(event) => void loadServer(event.currentTarget.value)}
      >
        <option value="">Select a server</option>
        {#each servers.data ?? [] as item (item.id)}<option value={item.id}
            >{item.name}</option
          >{/each}
      </select></label
    >
    {#if $state.server}
      <form
        onsubmit={(event) => {
          event.preventDefault();
          void saveConfiguration();
        }}
      >
        <label
          >Client ID <input
            required
            value={$state.fields.clientID}
            oninput={(event) => setField('clientID', event.currentTarget.value)}
          /></label
        >
        <label
          >Audience <input
            required
            value={$state.fields.audience}
            oninput={(event) => setField('audience', event.currentTarget.value)}
          /></label
        >
        <label
          >Scopes <input
            required
            value={$state.fields.scopes}
            oninput={(event) => setField('scopes', event.currentTarget.value)}
          /></label
        >
        <label
          >Authorization endpoint <input
            type="url"
            disabled={Boolean($state.fields.entraIssuer.trim())}
            required={!$state.fields.entraIssuer.trim()}
            value={$state.fields.authorizationEndpoint}
            oninput={(event) =>
              setField('authorizationEndpoint', event.currentTarget.value)}
          /></label
        >
        <label
          >Token endpoint <input
            type="url"
            required
            value={$state.fields.tokenEndpoint}
            oninput={(event) =>
              setField('tokenEndpoint', event.currentTarget.value)}
          /></label
        >
        <label
          >Device authorization endpoint <input
            type="url"
            disabled={Boolean($state.fields.entraIssuer.trim())}
            value={$state.fields.deviceEndpoint}
            oninput={(event) =>
              setField('deviceEndpoint', event.currentTarget.value)}
          /></label
        >
        <fieldset>
          <legend>Entra on-behalf-of authorization</legend>
          <p>
            Use a registered workload issuer and the middle-tier client ID.
            Target scopes must start with the audience. This mode requires a
            verified delegated user request and cannot combine with local
            delegated signing.
          </p>
          <label
            >Trusted Entra issuer <input
              type="url"
              value={$state.fields.entraIssuer}
              oninput={(event) =>
                setField('entraIssuer', event.currentTarget.value)}
            /></label
          >
          <label
            >Middle-tier client secret <input
              type="password"
              autocomplete="new-password"
              value={$state.fields.entraClientSecret}
              oninput={(event) =>
                setField('entraClientSecret', event.currentTarget.value)}
            /></label
          >
          <p>
            Leave the secret empty to retain the installed value. Clear the
            issuer to use browser authorization.
          </p>
        </fieldset>
        <fieldset>
          <legend>Delegated identity</legend>
          <p>
            Leave the issuer empty to disable signing. The private key stays in
            this installation.
          </p>
          <label
            >Delegated issuer <input
              type="url"
              value={$state.fields.delegatedIssuer}
              oninput={(event) =>
                setField('delegatedIssuer', event.currentTarget.value)}
            /></label
          >
          <label
            >Delegated audience <input
              value={$state.fields.delegatedAudience}
              oninput={(event) =>
                setField('delegatedAudience', event.currentTarget.value)}
            /></label
          >
          <label
            >PKCS8 Ed25519 signing key <textarea
              autocomplete="off"
              spellcheck="false"
              value={$state.fields.signingKey}
              oninput={(event) =>
                setField('signingKey', event.currentTarget.value)}
            ></textarea></label
          >
        </fieldset>
        <button disabled={$state.busy}>Save authorization settings</button>
      </form>
      {#if $state.server.oauth?.entra_obo}<p>
          Entra grants are created by verified delegated user requests. Browser
          enrollment is unavailable for this server.
        </p>{/if}
      <form
        onsubmit={(event) => {
          event.preventDefault();
          void startEnrollment();
        }}
      >
        <label
          >API key UUID <input
            required
            value={$state.fields.keyID}
            oninput={(event) => setField('keyID', event.currentTarget.value)}
          /></label
        >
        <button
          disabled={$state.busy ||
            !$state.server.oauth ||
            Boolean($state.server.oauth.entra_obo)}>Start key enrollment</button
        >
      </form>
      {#if $state.enrollmentURL}<p>
          <a
            href={$state.enrollmentURL}
            onclick={(event) => {
              event.preventDefault();
              consumeEnrollment($state.enrollmentURL.split('#')[1] ?? '');
            }}>Continue personal enrollment</a
          >
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
                  disabled={$state.busy}
                  onclick={() => revoke(grant.grant_id!)}>Revoke</button
                >{/if}</td
            >
          </tr>
        {/each}</tbody
      >
    </table>
  {/if}
</section>
