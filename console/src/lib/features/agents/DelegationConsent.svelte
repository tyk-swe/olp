<script lang="ts">
  import { onMount } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { apiKeyKeys } from '$lib/features/access/api-keys/apiKeyKeys';
  import { listApiKeys } from '$lib/features/access/api-keys/api';
  import { profileKeys } from '$lib/features/access/profile/profileKeys';
  import {
    getProfile,
    reauthenticateWithPassword,
    beginIdentityReauthentication
  } from '$lib/features/access/profile/api';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { usePrincipalLifetime } from '$lib/forms/mutationIntent.svelte';
  import { agentKeys } from './agentKeys';
  import {
    createDelegationConsent,
    getDelegationOptions,
    type DelegationConsent,
    type DelegationConsentRequest
  } from './api';

  const role = useRole();
  const inventory = createQuery(() => ({
    queryKey: apiKeyKeys.list(),
    queryFn: ({ signal }) => listApiKeys(signal),
    enabled: role.allows('POST /api/v1/delegation-consents')
  }));
  const profile = createQuery(() => ({
    queryKey: profileKeys.current(),
    queryFn: getProfile,
    enabled: role.allows('POST /api/v1/delegation-consents')
  }));
  let keyID = $state('');
  let agentID = $state('');
  let routeInput = $state('');
  let toolsetInput = $state('');
  let remoteAgentInput = $state('');
  let password = $state('');
  let verifiedKey = $state('');
  let busy = $state(false);
  let error = $state('');
  let consent = $state<DelegationConsent | null>(null);
  const owner = usePrincipalLifetime(() => {
    keyID = '';
    agentID = '';
    routeInput = '';
    toolsetInput = '';
    remoteAgentInput = '';
    password = '';
    verifiedKey = '';
    consent = null;
    error = '';
    busy = false;
  });
  const manageableKeys = $derived(
    (inventory.data ?? []).filter(
      (key) =>
        key.project_id &&
        !key.workload_issuer_id &&
        !key.revoked_at &&
        key.scopes.includes('delegate') &&
        (profile.data?.access_scope === 'global' ||
          profile.data?.projects?.some(
            (project) =>
              project.id === key.project_id && project.role === 'manager'
          ))
    )
  );
  const selectedKey = $derived(manageableKeys.find((key) => key.id === keyID));
  const options = createQuery(() => ({
    queryKey: agentKeys.delegationOptions(keyID),
    queryFn: ({ queryKey, signal }) =>
      getDelegationOptions(queryKey[1], signal),
    enabled: !!selectedKey && role.allows('GET /api/v1/delegation-options')
  }));
  const agents = $derived(
    (options.data?.agents ?? []).flatMap((option) => {
      const agent = inventory.data?.find((key) => key.id === option.id);
      return agent
        ? [
            {
              ...agent,
              eligibleRoutes: option.allowed_routes,
              eligibleToolsets: option.allowed_toolsets ?? [],
              eligibleAgents: option.allowed_agents ?? []
            }
          ]
        : [];
    })
  );
  const selectedAgent = $derived(agents.find((agent) => agent.id === agentID));
  const routes = $derived(
    routeInput
      .split(',')
      .map((route) => route.trim())
      .filter(Boolean)
  );
  const toolsets = $derived(
    toolsetInput
      .split(',')
      .map((toolset) => toolset.trim())
      .filter(Boolean)
  );
  const remoteAgents = $derived(
    remoteAgentInput
      .split(',')
      .map((agent) => agent.trim())
      .filter(Boolean)
  );
  const routesAllowed = $derived(
    routes.every(
      (route, index) =>
        selectedAgent?.eligibleRoutes.includes(route) &&
        routes.indexOf(route) === index
    )
  );
  const toolsetsAllowed = $derived(
    toolsets.every(
      (toolset, index) =>
        selectedAgent?.eligibleToolsets.includes(toolset) &&
        toolsets.indexOf(toolset) === index
    )
  );
  const remoteAgentsAllowed = $derived(
    remoteAgents.every(
      (agent, index) =>
        selectedAgent?.eligibleAgents.includes(agent) &&
        remoteAgents.indexOf(agent) === index
    )
  );
  const ready = $derived(
    !!selectedKey &&
      !!selectedAgent &&
      (routes.length > 0 || toolsets.length > 0 || remoteAgents.length > 0) &&
      routesAllowed &&
      toolsetsAllowed &&
      remoteAgentsAllowed
  );

  onMount(() => {
    const parameters = new URLSearchParams(window.location.search);
    if (parameters.get('reauthenticated') === 'agent_delegation') {
      verifiedKey = parameters.get('resource_id') ?? '';
      keyID = verifiedKey;
    }
  });

  async function confirm() {
    if (busy || !ready) return;
    const current = owner();
    const selected: DelegationConsentRequest = {
      api_key_id: keyID,
      agent_id: agentID,
      scopes: [
        ...(routes.length ? (['inference'] as const) : []),
        ...(toolsets.length ? (['tools'] as const) : []),
        ...(remoteAgents.length ? (['agents'] as const) : [])
      ],
      ...(routes.length ? { allowed_routes: routes } : {}),
      ...(toolsets.length ? { allowed_toolsets: toolsets } : {}),
      ...(remoteAgents.length ? { allowed_agents: remoteAgents } : {})
    };
    busy = true;
    error = '';
    consent = null;
    try {
      if (verifiedKey !== selected.api_key_id) {
        await reauthenticateWithPassword(
          password,
          'agent_delegation',
          selected.api_key_id
        );
        if (!current()) return;
        password = '';
      }
      const result = await createDelegationConsent(selected);
      if (!current()) return;
      verifiedKey = '';
      consent = result;
    } catch (cause) {
      if (current())
        error = errorMessage(cause, 'Could not authorize this agent.');
    } finally {
      if (current()) busy = false;
    }
  }

  async function verifyIdentity() {
    if (busy || !selectedKey) return;
    const current = owner();
    busy = true;
    error = '';
    try {
      const url = await beginIdentityReauthentication(
        'agent_delegation',
        selectedKey.id
      );
      if (current()) window.location.assign(url);
    } catch (cause) {
      if (current())
        error = errorMessage(cause, 'Could not start identity verification.');
    } finally {
      if (current()) busy = false;
    }
  }
</script>

{#if role.allows('POST /api/v1/delegation-consents')}
  <section class="card" aria-labelledby="agent-consent-heading">
    <h2 id="agent-consent-heading">Allow an agent to use a key</h2>
    <p>
      Choose a key you manage and an agent-enabled workload in the same project.
      Access is limited by both policies.
    </p>
    {#if inventory.isPending || profile.isPending}
      <p role="status">Loading keys and project access…</p>
    {:else if inventory.isError || profile.isError}
      <p role="alert">Keys or project access could not be loaded.</p>
    {:else}
      <div class="form-field">
        <label
          >Consumer key
          <select id="delegation-key" bind:value={keyID} disabled={busy}>
            <option value="">Choose a key</option>
            {#each manageableKeys as key (key.id)}<option value={key.id}
                >{key.name} ({key.project_name ?? key.project_id})</option
              >{/each}
          </select>
        </label>
      </div>
      <div class="form-field">
        <label
          >Agent workload
          <select
            id="delegation-agent"
            bind:value={agentID}
            disabled={busy || !selectedKey || options.isPending}
          >
            <option value="">Choose a workload</option>
            {#each agents as agent (agent.id)}<option value={agent.id}
                >{agent.name} ({agent.workload_mapping})</option
              >{/each}
          </select>
        </label>
      </div>
      <div class="form-field">
        <label
          >Allowed inference routes
          <input
            bind:value={routeInput}
            disabled={busy}
            placeholder="route-one, route-two"
          />
        </label>
      </div>
      <div class="form-field">
        <label
          >Allowed toolsets
          <input
            bind:value={toolsetInput}
            disabled={busy}
            placeholder="toolset UUIDs separated by commas"
          />
        </label>
      </div>
      <div class="form-field">
        <label
          >Allowed A2A agents
          <input
            bind:value={remoteAgentInput}
            disabled={busy}
            placeholder="agent UUIDs separated by commas"
          />
        </label>
      </div>
      {#if selectedKey && options.isError}
        <p role="alert">Eligible agents and resources could not be loaded.</p>
      {:else if selectedKey && options.isSuccess && agents.length === 0}
        <p>
          No eligible agents have access to a route, toolset or agent for this
          key
        </p>
      {/if}
      {#if selectedAgent}
        <p>
          Available routes {selectedAgent.eligibleRoutes.join(', ') || 'None'}
        </p>
        <p>
          Available toolsets {selectedAgent.eligibleToolsets.join(', ') ||
            'None'}
        </p>
        <p>
          Available A2A agents {selectedAgent.eligibleAgents.join(', ') ||
            'None'}
        </p>
        <p>
          Enter unique route names, toolset UUIDs and agent UUIDs separated by
          commas
        </p>
        {#if routes.length > 0 && !routesAllowed}
          <p role="alert">Choose only routes listed above.</p>
        {/if}
        {#if toolsets.length > 0 && !toolsetsAllowed}
          <p role="alert">Choose only toolsets listed above.</p>
        {/if}
        {#if remoteAgents.length > 0 && !remoteAgentsAllowed}
          <p role="alert">Choose only A2A agents listed above</p>
        {/if}
      {/if}
      {#if ready}
        <p>
          Allow {selectedAgent?.name} to use {selectedKey?.name} in {selectedKey?.project_name ??
            selectedKey?.project_id}
        </p>
        {#if routes.length}<p>Inference routes {routes.join(', ')}</p>{/if}
        {#if toolsets.length}<p>Toolsets {toolsets.join(', ')}</p>{/if}
        {#if remoteAgents.length}
          <p>A2A agents {remoteAgents.join(', ')}</p>
          <p>OLP cannot count or stop hidden remote iterations</p>
        {/if}
        <p>
          The key pays for the request. The consent expires after five minutes
          and can be used once
        </p>
      {/if}
      {#if verifiedKey === keyID && verifiedKey}
        <p role="status">
          Identity verified. Select the workload and resources again, then
          confirm within five minutes.
        </p>
      {:else}
        <div class="form-field">
          <label
            >Current password
            <input
              type="password"
              autocomplete="current-password"
              bind:value={password}
              disabled={busy}
            />
          </label>
        </div>
        <button
          type="button"
          disabled={busy || !selectedKey}
          onclick={verifyIdentity}>Verify with linked identity</button
        >
      {/if}
      <button type="button" disabled={busy || !ready} onclick={confirm}
        >Confirm agent access</button
      >
      {#if error}<p role="alert">{error}</p>{/if}
      {#if consent}
        <p role="status">
          Copy this one-use consent code to the selected agent through your
          approved channel. It expires at {consent.expires_at}.
        </p>
        <label>Consent code <input readonly value={consent.consent} /></label>
        <div class="form-field">
          <label>
            Pair budget ID
            <input readonly size="64" value={consent.delegation_pair_digest} />
          </label>
        </div>
      {/if}
    {/if}
  </section>
{/if}
