<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    listDefinitions,
    listPolicies,
    listAttachments,
    listProjects,
    saveDefinition as saveDefinitionAPI,
    savePolicy as savePolicyAPI,
    activateDefinition,
    activatePolicy,
    attachPolicy as attachPolicyAPI,
    evaluatePolicy as evaluatePolicyAPI,
    type Definition,
    type Policy,
    type Entry,
    type Result
  } from './api';
  import { guardrailKeys } from './guardrailKeys';
  const guardrailTypes = [
    'builtin.regex',
    'builtin.pii',
    'builtin.secrets',
    'builtin.keywords',
    'builtin.schema',
    'builtin.tools',
    'builtin.code_execution',
    'builtin.tool_permission',
    'webhook',
    'bedrock',
    'azure.content_safety',
    'azure.prompt_shields',
    'model_armor',
    'lakera',
    'presidio',
    'model',
    'plugin',
    'aim',
    'akto',
    'alice',
    'aporia',
    'cato_networks',
    'cisco_ai_defense',
    'compresr',
    'conduct',
    'crowdstrike_aidr',
    'deepkeep',
    'dynamoai',
    'enkryptai',
    'generic_guardrail_api',
    'grayswan',
    'guardrails_ai',
    'headroom',
    'hiddenlayer',
    'ibm_guardrails',
    'javelin',
    'lakera_v1',
    'lasso',
    'microsoft_purview',
    'noma',
    'noma_v2',
    'onyx',
    'openai_moderation',
    'ovalix',
    'pangea',
    'panw_prisma_airs',
    'pillar',
    'prompt_security',
    'promptguard',
    'qostodian_nexus',
    'qualifire',
    'repelloai',
    'rubrik',
    'singulr',
    'straiker',
    'vigil_guard',
    'xecguard',
    'zscaler_ai_guard',
    'semantic_guard',
    'decision_model'
  ] as const satisfies readonly Definition['type'][];
  const access = useRole();
  const client = useQueryClient();
  const definitions = createQuery(() => ({
    queryKey: guardrailKeys.definitions(),
    queryFn: listDefinitions
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
    queryKey: guardrailKeys.projects(),
    notifyOnChangeProps: 'all',
    queryFn: listProjects
  }));
  let tab = $state('Definitions');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  let selected = $state<Definition | null>(null);
  let name = $state('');
  let project = $state('');
  let type = $state<Definition['type']>('builtin.keywords');
  let configuration = $state('{"words":["restricted"]}');
  let timeout = $state(1000);
  let onError = $state<'fail_closed' | 'fail_open'>('fail_closed');
  let egress = $state(false);
  let selectedPolicy = $state<Policy | null>(null);
  let policyName = $state('');
  let entries = $state<Entry[]>([]);
  let attachPolicy = $state('');
  let scope = $state<
    'installation' | 'organization' | 'project' | 'route' | 'api-key'
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
      !['model', 'semantic_guard', 'decision_model', 'plugin'].includes(type)
  );
  const canEdit = $derived(access.allows('POST /api/v1/guardrails'));
  async function run(action: () => Promise<void>) {
    busy = true;
    error = notice = '';
    try {
      await action();
      await client.invalidateQueries({ queryKey: guardrailKeys.root });
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
  function edit(value: Definition | null) {
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
  }
  async function saveDefinition(event: SubmitEvent) {
    event.preventDefault();
    await run(async () => {
      const config = JSON.parse(configuration) as Record<string, unknown>;
      if (external) config.content_egress = egress;
      selected = await saveDefinitionAPI(selected, project, type, {
        name,
        configuration: config,
        timeout_ms: timeout,
        on_error: onError
      });
      notice = 'Draft saved. Activate its revision when it is ready.';
    });
  }
  function editPolicy(value: Policy | null) {
    selectedPolicy = value;
    policyName = value?.name ?? '';
    project = value?.project_id ?? project;
    entries = value?.entries.map((entry) => ({ ...entry })) ?? [];
  }
  async function savePolicy(event: SubmitEvent) {
    event.preventDefault();
    await run(async () => {
      selectedPolicy = await savePolicyAPI(selectedPolicy, project, {
        name: policyName,
        entries
      });
      notice = 'Policy draft saved with its ordered, pinned revisions.';
    });
  }
  function chooseDefinition(index: number, id: string) {
    const definition = definitions.data?.items.find((value) => value.id === id);
    if (definition)
      entries[index] = {
        ...entries[index],
        guardrail_id: id,
        revision_id: definition.revision_id
      };
  }
  async function attach(event: SubmitEvent) {
    event.preventDefault();
    await run(async () => {
      await attachPolicyAPI({
        scope,
        scope_id: scope === 'installation' ? 'installation' : scopeID,
        policy_revision_id: attachPolicy,
        order
      });
      notice = 'Policy attached. Inherited policies continue to apply.';
    });
  }
  async function evaluate(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = '';
    result = null;
    try {
      result = await evaluatePolicyAPI({
        policy_revision_id: evaluatePolicy,
        phase,
        segments: [{ id: 'text', role: 'user', text }]
      });
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<svelte:head><title>Guardrails · OpenLLMProxy</title></svelte:head>
<header class="page-header">
  <h1>Guardrails</h1>
  <p>
    Define safeguards, pin policy revisions, and apply them across your gateway.
  </p>
</header>
<nav aria-label="Guardrail sections" class="tabs">
  {#each ['Definitions', 'Policies', 'Attachments', 'Playground'] as section (section)}
    <button
      type="button"
      aria-current={tab === section ? 'page' : undefined}
      onclick={() => {
        tab = section;
        error = notice = '';
      }}>{section}</button
    >
  {/each}
</nav>
{#if error}<p role="alert" class="error">{error}</p>{/if}
{#if notice}<p role="status">{notice}</p>{/if}
{#if definitions.isError || policies.isError || attachments.isError}<p
    role="alert"
  >
    Guardrails could not be loaded. Reload to try again.
  </p>{/if}

{#if tab === 'Definitions'}
  <section class="panel">
    <h2>Definitions</h2>
    <ul>
      {#each definitions.data?.items ?? [] as value (value.id)}<li>
          <button onclick={() => edit(value)}>{value.name}</button>
          <span
            >{value.type} · Revision {value.revision} · {value.active_revision_id ===
            value.revision_id
              ? 'Active'
              : 'Draft'}</span
          >
        </li>{/each}
    </ul>
    {#if canEdit}<button onclick={() => edit(null)}>New definition</button>
      <form onsubmit={saveDefinition}>
        <label>Name<input required maxlength="100" bind:value={name} /></label>
        <label
          >Project<select required bind:value={project} disabled={!!selected}
            ><option value="">Choose a project</option
            >{#each projects.data?.items ?? [] as value (value.id)}<option
                value={value.id}>{value.name}</option
              >{/each}</select
          ></label
        >
        <label
          >Type<select bind:value={type} disabled={!!selected}
            >{#each guardrailTypes as value (value)}<option {value}
                >{value}</option
              >{/each}</select
          ></label
        >
        <label
          >Configuration (JSON)<textarea
            required
            rows="8"
            spellcheck="false"
            bind:value={configuration}></textarea></label
        >
        {#if external}<label class="check"
            ><input type="checkbox" required bind:checked={egress} />I
            acknowledge that this guardrail sends inspected content to its
            configured endpoint.</label
          >{/if}
        <label
          >Timeout (milliseconds)<input
            type="number"
            min="1"
            max="300000"
            required
            bind:value={timeout}
          /></label
        >
        <label
          >On evaluation error<select bind:value={onError}
            ><option value="fail_closed">Block the request</option><option
              value="fail_open">Continue and record the failure</option
            ></select
          ></label
        >
        <button disabled={busy} type="submit">Save draft</button>
        {#if selected}<button
            disabled={busy}
            type="button"
            onclick={() =>
              run(async () => {
                if (!selected) return;
                await activateDefinition(selected);
                notice = 'Definition revision activated.';
                edit(null);
              })}>Activate revision {selected.revision}</button
          >{/if}
      </form>
    {/if}
  </section>
{:else if tab === 'Policies'}
  <section class="panel">
    <h2>Ordered policies</h2>
    <ul>
      {#each policies.data?.items ?? [] as value (value.policy_id)}<li>
          <button onclick={() => editPolicy(value)}>{value.name}</button> ·
          Revision {value.revision}
        </li>{/each}
    </ul>
    {#if canEdit}<button onclick={() => editPolicy(null)}>New policy</button>
      <form onsubmit={savePolicy}>
        <label
          >Name<input required maxlength="100" bind:value={policyName} /></label
        >
        <label
          >Project<select
            required
            bind:value={project}
            disabled={!!selectedPolicy}
            ><option value="">Choose a project</option
            >{#each projects.data?.items ?? [] as value (value.id)}<option
                value={value.id}>{value.name}</option
              >{/each}</select
          ></label
        >
        <ol>
          {#each entries as entry, i (i)}<li class="entry">
              <label
                >Guardrail<select
                  required
                  value={entry.guardrail_id}
                  onchange={(event) =>
                    chooseDefinition(i, event.currentTarget.value)}
                  ><option value="">Choose a definition</option
                  >{#each (definitions.data?.items ?? []).filter((value) => value.project_id === project) as value (value.id)}<option
                      value={value.id}
                      >{value.name} · Revision {value.revision}</option
                    >{/each}</select
                ></label
              >
              <label
                >Phase<select bind:value={entry.phase}
                  >{#each ['input', 'input_parallel', 'output', 'output_stream'] as value (value)}<option
                      {value}>{value}</option
                    >{/each}</select
                ></label
              >
              <label
                >Action<select bind:value={entry.action}
                  >{#each ['block', 'redact', 'mask', 'annotate'] as value (value)}<option
                      {value}>{value}</option
                    >{/each}</select
                ></label
              >
              <label class="check"
                ><input type="checkbox" bind:checked={entry.monitor} />Monitor
                only</label
              ><label class="check"
                ><input
                  type="checkbox"
                  bind:checked={entry.mandatory}
                />Mandatory</label
              >
              <button
                type="button"
                onclick={() => {
                  entries = entries.filter((_, index) => index !== i);
                }}>Remove entry {i + 1}</button
              >
            </li>{/each}
        </ol>
        <button
          type="button"
          onclick={() => {
            entries = [
              ...entries,
              {
                guardrail_id: '',
                revision_id: '',
                phase: 'input',
                action: 'block',
                monitor: false,
                mandatory: true
              }
            ];
          }}>Add guardrail</button
        >
        <button type="submit" disabled={busy}>Save policy draft</button>
        {#if selectedPolicy}<button
            type="button"
            disabled={busy}
            onclick={() =>
              run(async () => {
                if (!selectedPolicy) return;
                await activatePolicy(selectedPolicy);
                notice =
                  'Policy activated. Existing attachments keep their pinned revision.';
                editPolicy(null);
              })}>Activate revision</button
          >{/if}
      </form>{/if}
  </section>
{:else if tab === 'Attachments'}
  <section class="panel">
    <h2>Inherited safeguards</h2>
    <p>
      Policies run in this order: installation, organization, project, route,
      then key. Each attachment adds safeguards; narrower scopes cannot remove
      an inherited policy.
    </p>
    <ul>
      {#each attachments.data?.items ?? [] as value (value.id)}<li>
          {value.scope} · {value.scope_id} · Position {value.order} · Policy revision
          {value.policy_revision_id}
        </li>{/each}
    </ul>
    {#if canEdit}<form onsubmit={attach}>
        <label
          >Policy revision<select required bind:value={attachPolicy}
            ><option value="">Choose a policy</option
            >{#each policies.data?.items ?? [] as value (value.policy_id)}<option
                value={value.revision_id}
                >{value.name} · Revision {value.revision}</option
              >{/each}</select
          ></label
        >
        <label
          >Scope<select bind:value={scope}
            >{#each ['installation', 'organization', 'project', 'route', 'api-key'] as value (value)}<option
                {value}>{value}</option
              >{/each}</select
          ></label
        >
        {#if scope !== 'installation'}<label
            >Scope identifier<input required bind:value={scopeID} /></label
          >{/if}
        <label
          >Position<input
            type="number"
            min="0"
            required
            bind:value={order}
          /></label
        ><button disabled={busy}>Attach policy</button>
      </form>{/if}
  </section>
{:else}
  <section class="panel">
    <h2>Guardrail playground</h2>
    <p>
      Test a policy without saving the content you enter. External guardrails
      send it to their configured endpoints.
    </p>
    <form onsubmit={evaluate}>
      <label
        >Policy revision<select required bind:value={evaluatePolicy}
          ><option value="">Choose a policy</option
          >{#each policies.data?.items ?? [] as value (value.policy_id)}<option
              value={value.revision_id}
              >{value.name} · Revision {value.revision}</option
            >{/each}</select
        ></label
      >
      <label
        >Phase<select bind:value={phase}
          >{#each ['input', 'input_parallel', 'output', 'output_stream'] as value (value)}<option
              {value}>{value}</option
            >{/each}</select
        ></label
      >
      <label
        >Text<textarea rows="8" required bind:value={text}></textarea></label
      ><button
        disabled={busy || !access.allows('POST /api/v1/guardrails/evaluate')}
        >Evaluate</button
      ><button
        type="button"
        onclick={() => {
          text = '';
          result = null;
        }}>Clear content</button
      >
    </form>
    {#if result}<div role="status">
        <h3>{result.blocked ? 'Blocked' : 'Allowed'}</h3>
        <ul>
          {#each result.decisions as decision (`${decision.guardrail_id}-${decision.phase}-${decision.outcome}`)}<li
            >
              {decision.guardrail_id} · {decision.phase} · {decision.outcome} · {(
                decision.categories ?? []
              ).join(', ')}
            </li>{/each}
        </ul>
        <pre>{result.segments.map((segment) => segment.text).join('\n')}</pre>
      </div>{/if}
  </section>
{/if}

<style>
  .tabs {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    margin-block: 1rem;
  }
  .tabs [aria-current] {
    font-weight: 700;
    text-decoration: underline;
  }
  .panel {
    padding: 1.5rem;
    border: 1px solid var(--border, #d7dce2);
    border-radius: 0.75rem;
  }
  form {
    display: grid;
    gap: 1rem;
    max-width: 52rem;
    margin-top: 1rem;
  }
  label {
    display: grid;
    gap: 0.3rem;
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .entry {
    margin-block: 1rem;
    padding: 1rem;
    border: 1px solid var(--border, #d7dce2);
  }
  textarea {
    width: 100%;
    resize: vertical;
  }
  pre {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .error {
    color: var(--danger, #b42318);
  }
</style>
