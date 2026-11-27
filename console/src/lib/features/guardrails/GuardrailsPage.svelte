<script lang="ts">
  import { useAction } from '#lib/editing/action.svelte.js';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import CredentialsPanel from './CredentialsPanel.svelte';
  import GuardrailAttachments from './GuardrailAttachments.svelte';
  import GuardrailDefinitionDialog from './GuardrailDefinitionDialog.svelte';
  import GuardrailDefinitionTable from './GuardrailDefinitionTable.svelte';
  import GuardrailEvaluation from './GuardrailEvaluation.svelte';
  import GuardrailPolicyDialog from './GuardrailPolicyDialog.svelte';
  import GuardrailPolicyTable from './GuardrailPolicyTable.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { listProjects, projectKeys } from '#lib/features/access/index.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    listCredentials,
    retireDefinition,
    retirePolicy,
    listDefinitions,
    listPolicies,
    listAttachments,
    type Definition,
    type Policy,
    type Entry,
    type Result
  } from './api';
  import { guardrailKeys } from './guardrailKeys';

  const phases = [
    'input',
    'input_parallel',
    'output',
    'output_stream'
  ] as const satisfies readonly Entry['phase'][];
  const access = useRole();
  const client = useQueryClient();
  const definitions = createQuery(() => ({
    queryKey: guardrailKeys.definitions(),
    queryFn: ({ signal }) => listDefinitions(signal)
  }));
  const policies = createQuery(() => ({
    queryKey: guardrailKeys.policies(),
    notifyOnChangeProps: 'all',
    queryFn: listPolicies
  }));
  const attachments = createQuery(() => ({
    queryKey: guardrailKeys.attachments(),
    notifyOnChangeProps: 'all',
    queryFn: listAttachments
  }));
  const projects = createQuery(() => ({
    queryKey: projectKeys.all,
    notifyOnChangeProps: 'all',
    queryFn: ({ signal }) => listProjects(signal)
  }));
  const credentials = createQuery(() => ({
    queryKey: guardrailKeys.credentials(),
    queryFn: listCredentials
  }));
  let credentialID = $state('');
  let creatingCredential = $state(false);
  const definitionIntent = useMutationIntent();
  const definitionActivationIntent = useMutationIntent();
  const policyIntent = useMutationIntent();
  const policyActivationIntent = useMutationIntent();
  const attachIntent = useMutationIntent();
  const capture = usePrincipalLifetime(() => {
    edit(null);
    editPolicy(null);
    configuration = '{}';
    text = '';
    result = null;
  });
  let {
    tab
  }: { tab: 'checks' | 'policies' | 'attachments' | 'credentials' | 'try' } =
    $props();
  const action = useAction();
  let evaluating = $state(false);
  const busy = $derived(action.busy || evaluating);
  let error = $state('');
  let definitionOpen = $state(false);
  let policyOpen = $state(false);
  let definitionEditor = $state(0);
  let selected = $state<Definition | null>(null);
  let name = $state('');
  let project = $state('');
  let type = $state<Definition['type']>('builtin.keywords');
  let configuration = $state('{"words":["restricted"]}');
  let timeout = $state(1000);
  let onError = $state<'fail_closed' | 'fail_open'>('fail_closed');
  let egress = $state(false);
  let policyEditor = $state(0);
  let selectedPolicy = $state<Policy | null>(null);
  let policyName = $state('');
  let entries = $state<Entry[]>([]);
  let attachPolicy = $state('');
  let scope = $state<
    | 'installation'
    | 'organization'
    | 'project'
    | 'route'
    | 'toolset'
    | 'api-key'
  >('project');
  let scopeID = $state('');
  let order = $state(0);
  let evaluatePolicy = $state('');
  let phase = $state<Entry['phase']>('input');
  // Content stays in component memory and never enters a query cache or storage.
  let text = $state('');
  let result = $state<Result | null>(null);
  const external = $derived(
    !type.startsWith('builtin.') &&
      ![
        'model',
        'semantic_guard',
        'decision_model',
        'typesafe',
        'plugin'
      ].includes(type)
  );
  const canEdit = $derived(access.allows('POST /api/v1/guardrails'));
  async function refresh(current: () => boolean) {
    if (current())
      await client.invalidateQueries({ queryKey: guardrailKeys.root });
  }
  function openDefinition(value: Definition | null) {
    error = '';
    edit(value);
    definitionOpen = true;
  }
  function closeDefinition() {
    edit(null);
    definitionOpen = false;
  }
  function openPolicy(value: Policy | null) {
    error = '';
    editPolicy(value);
    policyOpen = true;
  }
  function closePolicy() {
    editPolicy(null);
    policyOpen = false;
  }
  function edit(value: Definition | null) {
    definitionEditor += 1;
    selected = value;
    name = value?.name ?? '';
    project = value?.project_id ?? project;
    type = value?.type ?? 'builtin.keywords';
    configuration = JSON.stringify(
      value?.configuration ?? { words: ['restricted'] },
      null,
      2
    );
    timeout = value?.timeout_ms ?? 1000;
    onError = value?.on_error ?? 'fail_closed';
    egress = value?.configuration?.content_egress === true;
    credentialID =
      typeof value?.configuration?.credential_ref === 'string'
        ? value.configuration.credential_ref
        : '';
    creatingCredential = false;
  }
  function editPolicy(value: Policy | null) {
    policyEditor += 1;
    selectedPolicy = value;
    policyName = value?.name ?? '';
    project = value?.project_id ?? project;
    entries = value?.entries.map((entry) => ({ ...entry })) ?? [];
  }
</script>

<PageTitle title="Guardrails" />
{#if error && !definitionOpen && !policyOpen}<Callout tone="danger">
    {error}
  </Callout>{/if}
{#if definitions.isPending || policies.isPending || projects.isPending}
  <div class="loading-state compact" role="status">
    Loading guardrail inventory…
  </div>
{/if}
{#if definitions.isError || policies.isError || attachments.isError || projects.isError}<Callout
    tone="danger"
  >
    Guardrails didn’t load. Reload the page to try again.
  </Callout>{/if}

{#if tab === 'checks'}
  <div class="toolbar">
    <p class="lede">
      A check inspects prompts or responses, for example for blocked words or
      personal data. Group checks into a policy to use them.
    </p>
    {#if canEdit}<div class="page-actions">
        <button
          class="button button-primary"
          onclick={() => openDefinition(null)}
        >
          New check
        </button>
      </div>{/if}
  </div>
  <GuardrailDefinitionTable
    pending={definitions.isPending}
    items={definitions.data ?? []}
    canRetire={access.allows('DELETE /api/v1/guardrails/{guardrail_id}')}
    onEdit={openDefinition}
    onRetire={retireDefinition}
    onRetired={async (value) => {
      if (selected?.id === value.id) closeDefinition();
      await definitions.refetch();
    }}
  />
  {#if definitionOpen}
    <GuardrailDefinitionDialog
      {busy}
      {external}
      projects={projects.data ?? []}
      credentials={credentials.data ?? []}
      bind:name
      bind:project
      bind:type
      bind:configuration
      bind:credentialID
      bind:timeout
      bind:egress
      bind:onError
      bind:creatingCredential
      {action}
      {refresh}
      {definitionIntent}
      {definitionActivationIntent}
      {definitionEditor}
      bind:error
      bind:selected
      onClose={closeDefinition}
    />
  {/if}
{:else if tab === 'policies'}
  <div class="toolbar">
    <p class="lede">
      A policy runs its checks in order. Attach a policy to apply it to an
      installation, project, route or key.
    </p>
    {#if canEdit}<div class="page-actions">
        <button class="button button-primary" onclick={() => openPolicy(null)}>
          New policy
        </button>
      </div>{/if}
  </div>
  <GuardrailPolicyTable
    pending={policies.isPending}
    items={policies.data?.items ?? []}
    canRetire={access.allows('DELETE /api/v1/guardrail-policies/{policy_id}')}
    onEdit={openPolicy}
    onRetire={retirePolicy}
    onRetired={async (value) => {
      if (selectedPolicy?.policy_id === value.policy_id) closePolicy();
      await policies.refetch();
    }}
  />
  {#if policyOpen}
    <GuardrailPolicyDialog
      {busy}
      {phases}
      definitions={definitions.data ?? []}
      projects={projects.data ?? []}
      bind:policyName
      bind:project
      bind:entries
      {action}
      {refresh}
      {policyIntent}
      {policyActivationIntent}
      {policyEditor}
      bind:error
      bind:selectedPolicy
      onClose={closePolicy}
    />
  {/if}
{:else if tab === 'attachments'}
  <GuardrailAttachments
    items={attachments.data?.items ?? []}
    policies={policies.data?.items ?? []}
    {canEdit}
    {busy}
    bind:attachPolicy
    bind:scope
    bind:scopeID
    bind:order
    {action}
    {refresh}
    {attachIntent}
    bind:error
    onChanged={async () => {
      await client.invalidateQueries({ queryKey: guardrailKeys.root });
    }}
  />
{:else if tab === 'credentials'}
  <CredentialsPanel projects={projects.data ?? []} />
{:else}
  <GuardrailEvaluation
    policies={policies.data?.items ?? []}
    {phases}
    {busy}
    canEvaluate={access.allows('POST /api/v1/guardrails/evaluate')}
    bind:evaluatePolicy
    bind:phase
    bind:text
    bind:result
    {capture}
    bind:evaluating
    bind:error
  />
{/if}
