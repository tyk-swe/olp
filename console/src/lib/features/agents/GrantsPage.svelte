<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { onMount } from 'svelte';
  import { writable } from 'svelte/store';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { agentKeys } from './agentKeys';
  import DelegationConsent from './DelegationConsent.svelte';
  import AgentAuthorizationForm from './AgentAuthorizationForm.svelte';
  import GrantStanding from './GrantStanding.svelte';
  import GrantsServerAuthorization from './GrantsServerAuthorization.svelte';
  import {
    getServer,
    listMCPGrants,
    listPublications,
    listServers,
    revokeMCPGrant,
    saveServer
  } from './api';

  import { PersonalGrantEnrollment } from './grantEnrollment.svelte';
  const enrollment = new PersonalGrantEnrollment();
  const grantView = $derived(enrollment.view);

  import { initialState, type Fields, type PageState } from './grantsState.js';
  const state = writable(initialState());
  const access = useRole();
  const client = useQueryClient();
  const saveMutation = useMutation();
  const revokeAction = useAction();
  const working = $derived(
    saveMutation.busy || revokeAction.busy || grantView.busy
  );
  const grants = createQuery(() => ({
    queryKey: agentKeys.grants(),
    queryFn: listMCPGrants
  }));
  const servers = createQuery(() => ({
    queryKey: agentKeys.servers(),
    queryFn: listServers,
    enabled: access.allows('GET /api/v1/mcp-servers')
  }));
  const agents = createQuery(() => ({
    queryKey: agentKeys.agents(),
    queryFn: () => listPublications('agents'),
    enabled: access.allows('GET /api/v1/agents')
  }));
  const canManage = $derived(
    access.allows('POST /api/v1/mcp-servers/{server_id}/grant-enrollments')
  );
  const canManageAgents = $derived(
    access.allows('POST /api/v1/agents/{agent_id}/grant-enrollments')
  );
  const captureOwner = usePrincipalLifetime(() => state.set(initialState()));
  const patch = (values: Partial<PageState>) =>
    state.update((current) => ({ ...current, ...values }));
  const setField = (field: keyof Fields, value: string) =>
    state.update((current) => ({
      ...current,
      fields: { ...current.fields, [field]: value }
    }));

  function consumeLocation(location: URL) {
    const sanitized = enrollment.consumeLocation(location);
    if (sanitized !== null) {
      patch({ error: '', message: '' });
      window.history.replaceState(null, '', sanitized);
    }
  }
  onMount(() => consumeLocation(new URL(window.location.href)));

  async function loadServer(requested: string) {
    const owner = captureOwner();
    const selection = { id: requested };
    saveMutation.reset();
    enrollment.reset();
    state.set({ ...initialState(), selection });
    if (!requested) return;
    const loaded = await readResult(() => getServer(requested));
    if (!owner() || $state.selection !== selection) return;
    if (!loaded.ok) {
      patch({ error: errorMessage(loaded.error) });
      return;
    }
    patch({
      server: loaded.value,
      fields: {
        delegatedIssuer: loaded.value.delegation?.issuer ?? '',
        delegatedAudience: loaded.value.delegation?.audience ?? '',
        signingKey: '',
        entraIssuer: loaded.value.oauth?.entra_obo?.issuer ?? '',
        entraClientSecret: '',
        clientID: loaded.value.oauth?.client_id ?? '',
        audience: loaded.value.oauth?.audience ?? '',
        scopes: loaded.value.oauth?.scopes.join(' ') ?? '',
        authorizationEndpoint: loaded.value.oauth?.authorization_endpoint ?? '',
        tokenEndpoint: loaded.value.oauth?.token_endpoint ?? '',
        deviceEndpoint: loaded.value.oauth?.device_authorization_endpoint ?? '',
        keyID: ''
      }
    });
  }

  async function refresh(
    queryKey:
      | ReturnType<typeof agentKeys.grants>
      | ReturnType<typeof agentKeys.servers>,
    active: () => boolean
  ) {
    const result = await readResult(() =>
      client.invalidateQueries({ queryKey }, { throwOnError: true })
    );
    if (!result.ok && active())
      patch({
        error: `Write saved, but refreshing failed. ${errorMessage(result.error)}`
      });
  }

  async function saveConfiguration() {
    const observed = $state;
    if (!observed.server || working) return;
    const owner = captureOwner();
    const active = () => owner() && $state.selection === observed.selection;
    const fields = observed.fields;
    patch({ error: '', message: '' });
    const committed = await saveMutation.submit(
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
        saveServer(submission.current, submission.input, requestId),
      async (saved, current) => {
        if (!active() || !current()) return;
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
      },
      { failure: (cause) => errorMessage(cause) }
    );
    if (active() && !committed) patch({ error: saveMutation.error });
  }

  async function startEnrollment() {
    if (!$state.server || working) return;
    patch({ error: '', message: '' });
    await enrollment.begin({
      kind: 'server',
      id: $state.server.id,
      keyId: $state.fields.keyID
    });
  }
  async function startAgentEnrollment() {
    if (working) return;
    patch({ error: '', message: '' });
    await enrollment.begin({
      kind: 'agent',
      id: $state.agentID,
      keyId: $state.agentKeyID
    });
  }

  async function runEnrollment(action: () => Promise<void>) {
    patch({ error: '', message: '' });
    await action();
  }

  async function revoke(grantID: string) {
    if (
      !(await confirmAction({
        title: 'Revoke this grant?',
        body: 'The agent can no longer act through this connection.',
        confirmLabel: 'Revoke grant',
        danger: true
      }))
    )
      return;
    const owner = captureOwner();
    patch({ error: '' });
    const committed = await revokeAction.run(
      () => revokeMCPGrant(grantID),
      async (_, current) => {
        if (owner() && current())
          await refresh(agentKeys.grants(), () => owner() && current());
      },
      { key: grantID, failure: (cause) => errorMessage(cause) }
    );
    if (owner() && !committed) patch({ error: revokeAction.error });
  }
</script>

<svelte:window
  onhashchange={() => consumeLocation(new URL(window.location.href))}
/>

<PageTitle title="Personal tool grants" />
<PageHeader
  title="My connections"
  description="Grant standing is bound to one individual and the resource's authorization settings."
/>
<DelegationConsent />
{#if $state.error || grantView.error}<p class="inline-problem" role="alert">
    {$state.error || grantView.error}
  </p>{/if}
{#if $state.message || grantView.message}<Callout tone="success">
    {$state.message || grantView.message}
  </Callout>{/if}
{#if grantView.handoffToken}
  <section class="section">
    <h2>Claim enrollment</h2>
    <p>This one-use link binds enrollment to your signed-in account</p>
    <button
      class="button button-secondary"
      disabled={working}
      onclick={() => runEnrollment(enrollment.claim)}
    >
      Claim enrollment
    </button>
  </section>
{/if}
{#if grantView.enrollmentID && !grantView.handoffToken && grantView.authorization?.state !== 'active'}
  <section class="section">
    <h2>Authorize personal access</h2>
    {#if grantView.callbackCode}
      <button
        class="button button-secondary"
        disabled={working}
        onclick={() => runEnrollment(enrollment.completeCallback)}
      >
        Complete authorization
      </button>
    {:else if grantView.authorization?.device}
      <p>
        Enter {grantView.authorization.device.user_code} at
        <a
          href={grantView.authorization.device.verification_url}
          target="_blank"
          rel="noreferrer"
        >
          the authorization page
        </a>
      </p>
      <p>Check again after {grantView.authorization.device.interval} seconds</p>
      <button
        class="button button-secondary"
        disabled={working}
        onclick={() => runEnrollment(enrollment.poll)}
      >
        Check device approval
      </button>
    {:else if grantView.authorization?.authorization_url}
      <a
        class="button button-primary"
        href={grantView.authorization.authorization_url}
      >
        Sign in upstream
      </a>
    {:else}
      <button
        class="button button-secondary"
        disabled={working}
        onclick={() =>
          runEnrollment(() => enrollment.authorize('authorization_code'))}
      >
        Authorize in browser
      </button>
      <button
        class="button button-secondary"
        disabled={working}
        onclick={() => runEnrollment(() => enrollment.authorize('device_code'))}
      >
        Use device authorization
      </button>
    {/if}
  </section>
{/if}

{#if canManage}
  <GrantsServerAuthorization
    pageState={state}
    servers={servers.data ?? []}
    {working}
    {setField}
    {loadServer}
    {saveConfiguration}
    {startEnrollment}
  />
{/if}
{#if canManageAgents}
  <section class="section">
    <h2>Agent authorization</h2>
    <AgentAuthorizationForm
      pageState={state}
      {agents}
      {working}
      {patch}
      {enrollment}
      {startAgentEnrollment}
    />
  </section>
{/if}
{#if grantView.enrollmentURL}<p>
    <a
      href={grantView.enrollmentURL}
      onclick={(event) => {
        event.preventDefault();
        consumeLocation(
          new URL(
            new URL(grantView.enrollmentURL, window.location.href).hash,
            window.location.href
          )
        );
      }}
    >
      Continue personal enrollment
    </a>
  </p>{/if}
<GrantStanding
  error={grants.error ? errorMessage(grants.error) : null}
  pending={grants.isPending}
  items={grants.data ?? []}
  {working}
  onRevoke={revoke}
/>

<style>
  section {
    display: grid;
    gap: 0.75rem;
  }
  section > :is(h2, .button) {
    justify-self: start;
  }
</style>
