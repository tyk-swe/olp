<script lang="ts">
  import { untrack } from 'svelte';
  import {
    saveCodeRoute,
    type CodeAccount,
    type CodePool,
    type CodeRoute,
    type CodeRouteWrite
  } from '$lib/api/code-mode';
  import { nativeModels, type PaidTargetOption } from './presentation';

  import {
    useResourceSubmission,
    type ResourceEditorProps
  } from './resourceSubmission.svelte';
  import ResourceEditorForm from './ResourceEditorForm.svelte';

  type PaidTargets = NonNullable<CodeRouteWrite['paid_targets']>;

  type AliasValidation =
    | { kind: 'valid'; aliases: Record<string, string> }
    | { kind: 'invalid'; message: string };

  function parseAliases(
    value: string,
    models: readonly string[],
    rewriting: boolean
  ): AliasValidation {
    const entries = value
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean)
      .map((line) => line.split('=').map((part) => part.trim()));
    if (
      !entries.every(
        (entry): entry is [string, string] =>
          entry.length === 2 &&
          !!entry[0] &&
          !!entry[1] &&
          /^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$/.test(entry[0]) &&
          !models.includes(entry[0]) &&
          models.includes(entry[1])
      )
    )
      return {
        kind: 'invalid',
        message:
          'Use one distinct alias=native-model per line, targeting a listed native model.'
      };
    if (entries.length && !rewriting)
      return {
        kind: 'invalid',
        message: 'Enable model rewriting to publish aliases.'
      };
    if (
      entries.length > 100 ||
      new Set(entries.map(([alias]) => alias)).size !== entries.length
    )
      return { kind: 'invalid', message: 'Use up to 100 unique aliases.' };
    return { kind: 'valid', aliases: Object.fromEntries(entries) };
  }

  type ModelPoolValidation =
    | { kind: 'valid'; pools: Record<string, string[]> }
    | { kind: 'invalid'; message: string };

  function parseModelPools(
    value: string,
    models: readonly string[],
    aliases: Readonly<Record<string, string>>,
    rewriting: boolean
  ): ModelPoolValidation {
    const lines = value
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);
    const entries = lines.map((line) =>
      line.split('=').map((part) => part.trim())
    );
    if (
      !entries.every(
        (entry): entry is [string, string] =>
          entry.length === 2 && !!entry[0] && !!entry[1]
      )
    )
      return {
        kind: 'invalid',
        message: 'Use public-model=primary,alternate per line.'
      };
    const pools = entries.map(
      ([publicName, targets]) =>
        [publicName, targets.split(',').map((target) => target.trim())] as const
    );
    if (pools.length && !rewriting)
      return {
        kind: 'invalid',
        message: 'Enable model rewriting to publish model pools.'
      };
    if (
      pools.length > 100 ||
      new Set(pools.map(([publicName]) => publicName)).size !== pools.length ||
      !pools.every(([publicName, targets]) => {
        const primary = models.includes(publicName)
          ? publicName
          : aliases[publicName];
        return (
          !!primary &&
          targets.length >= 2 &&
          targets.length <= 100 &&
          targets[0] === primary &&
          new Set(targets).size === targets.length &&
          targets.every((target) => models.includes(target))
        );
      })
    )
      return {
        kind: 'invalid',
        message:
          'Use distinct published native targets, starting with the public model’s normal exact target.'
      };
    return { kind: 'valid', pools: Object.fromEntries(pools) };
  }

  let {
    current,
    projectId,
    pools,
    providers = [],
    accounts,
    allowed,
    onDraftChange,
    onSaved,
    onCancel
  }: ResourceEditorProps<CodeRoute> & {
    pools: CodePool[];
    providers?: PaidTargetOption[];
    accounts: CodeAccount[];
  } = $props();
  const initial = untrack(() => current);
  let slug = $state(initial?.slug ?? '');
  let poolId = $state(initial?.pool_id ?? '');
  let maxBodyBytes = $state(initial?.max_body_bytes?.toString() ?? '');
  let models = $state(initial?.models.join('\n') ?? '');
  let paidParticipation = $state(initial?.paid_participation ?? false);
  let paidTargets = $state(initial?.paid_targets ?? {});
  let paidFallbackTargets = $state(initial?.paid_fallback_targets ?? {});
  let enabled = $state(initial?.enabled ?? true);
  let modelRewriting = $state(initial?.model_rewriting ?? false);
  let aliases = $state(
    Object.entries(initial?.aliases ?? {})
      .map(([alias, target]) => `${alias}=${target}`)
      .join('\n')
  );
  let modelPools = $state(
    Object.entries(initial?.model_pools ?? {})
      .map(([publicName, targets]) => `${publicName}=${targets.join(',')}`)
      .join('\n')
  );
  let recoveryAttempts = $state(initial?.initial_recovery_attempts ?? 0);
  let chatToCodex = $state(initial?.chat_to_codex ?? false);
  let messagesToCodex = $state(initial?.messages_to_codex ?? false);
  let codexCLIIdentity = $state(initial?.codex_cli_identity ?? false);
  let chatToAntigravity = $state(initial?.chat_to_antigravity ?? false);
  let research = $state(!!initial?.tools);
  let researchAccount = $state(initial?.tools?.account_id ?? '');
  let researchServices = $state<('search' | 'reader' | 'zread')[]>(
    initial?.tools?.services ?? ['search', 'reader', 'zread']
  );
  const services = [
    { id: 'search', label: 'Search' },
    { id: 'reader', label: 'Web reader' },
    { id: 'zread', label: 'Repository research' }
  ] as const;
  const researchAccounts = $derived(
    accounts.filter(
      (account) =>
        account.project_id === projectId &&
        account.research_eligible &&
        account.research_services?.length &&
        pools
          .find((pool) => pool.id === poolId)
          ?.account_ids.includes(account.id)
    )
  );
  $effect(() => {
    if (research && !researchAccount && researchAccounts.length === 1)
      researchAccount = researchAccounts[0].id;
  });

  const submission = useResourceSubmission({
    allowed: () => allowed,
    creating: () => !current,
    onDraftChange: (dirty) => onDraftChange(dirty),
    onSaved: () => onSaved(),
    onCancel: () => onCancel(),
    build: (): CodeRouteWrite => {
      if (!/^[a-z0-9][a-z0-9._-]{0,99}$/.test(slug))
        throw new Error(
          'Use a route slug of 1–100 lowercase letters, numbers, dots, underscores or hyphens, starting with a letter or number.'
        );
      if (
        maxBodyBytes &&
        (!/^\d+$/.test(maxBodyBytes) ||
          Number(maxBodyBytes) < 1 ||
          Number(maxBodyBytes) > 1073741824)
      )
        throw new Error(
          'Use a body limit from 1 to 1073741824 bytes, or leave it empty.'
        );
      if (
        research &&
        (!researchAccounts.some((account) => account.id === researchAccount) ||
          !researchServices.length)
      )
        throw new Error(
          'Choose an eligible research account and at least one service.'
        );
      const routeModels = nativeModels(models);
      const aliasResult = parseAliases(aliases, routeModels, modelRewriting);
      if (aliasResult.kind === 'invalid') throw new Error(aliasResult.message);
      const poolResult = parseModelPools(
        modelPools,
        routeModels,
        aliasResult.aliases,
        modelRewriting
      );
      if (poolResult.kind === 'invalid') throw new Error(poolResult.message);
      const selectedTargets = (
        values: NonNullable<CodeRouteWrite['paid_targets']>
      ): NonNullable<CodeRouteWrite['paid_targets']> =>
        paidParticipation
          ? Object.fromEntries(
              routeModels.flatMap((model) =>
                values[model]?.provider_id ? [[model, values[model]]] : []
              )
            )
          : {};
      const targets = selectedTargets(paidTargets);
      const fallbackTargets = selectedTargets(paidFallbackTargets);
      if (
        Object.values({ ...targets, ...fallbackTargets }).some(
          (target) => !target.slot_id
        )
      )
        throw new Error('Choose a credential slot for every paid API target.');
      if (Object.keys(fallbackTargets).length && !recoveryAttempts)
        throw new Error(
          'Enable bounded recovery before selecting paid fallback targets.'
        );
      if (Object.keys(fallbackTargets).some((model) => !!targets[model]))
        throw new Error(
          'Choose either direct paid selection or subscription-first fallback for each native model.'
        );
      return {
        paid_participation: paidParticipation,
        paid_targets: targets,
        paid_fallback_targets: fallbackTargets,
        project_id: projectId,
        slug,
        pool_id: poolId,
        max_body_bytes: maxBodyBytes ? Number(maxBodyBytes) : null,
        models: routeModels,
        aliases: aliasResult.aliases,
        model_rewriting: modelRewriting,
        model_pools: poolResult.pools,
        chat_to_codex: chatToCodex,
        messages_to_codex: messagesToCodex,
        codex_cli_identity: codexCLIIdentity,
        chat_to_antigravity: chatToAntigravity,
        initial_recovery_attempts: recoveryAttempts,
        tools: research
          ? { account_id: researchAccount, services: researchServices }
          : null,
        enabled
      };
    },
    persist: (input, requestId) => saveCodeRoute(input, current, requestId)
  });
</script>

{#snippet paidTargetSelectors(
  targets: PaidTargets,
  prefix: string,
  label: string,
  emptyLabel: string,
  onchange: (targets: PaidTargets) => void
)}
  {#each nativeModels(models) as model (model)}
    {@const target = targets[model]}
    <div class="form-field">
      <label for={prefix + '-provider-' + model}
        >{label} provider for {model}</label
      >
      <select
        id={prefix + '-provider-' + model}
        value={target?.provider_id ?? ''}
        onchange={(event) =>
          onchange({
            ...targets,
            [model]: { provider_id: event.currentTarget.value, slot_id: '' }
          })}
      >
        <option value="">{emptyLabel}</option>
        {#each providers as provider (provider.id)}<option value={provider.id}
            >{provider.name}</option
          >{/each}
        {#if target?.provider_id && !providers.some((provider) => provider.id === target.provider_id)}<option
            value={target.provider_id}
            >{target.provider_id} · current selection</option
          >{/if}
      </select>
      {#if target?.provider_id}
        <label for={prefix + '-slot-' + model}
          >{label} credential slot for {model}</label
        >
        <select
          id={prefix + '-slot-' + model}
          value={target.slot_id ?? ''}
          onchange={(event) =>
            onchange({
              ...targets,
              [model]: {
                provider_id: target.provider_id,
                slot_id: event.currentTarget.value
              }
            })}
          required
        >
          <option value="">Choose a credential slot</option>
          {#each providers.find((provider) => provider.id === target.provider_id)?.slots ?? [] as slot (slot.id)}<option
              value={slot.id}>{slot.name || slot.id}</option
            >{/each}
        </select>
      {/if}
    </div>
  {/each}
{/snippet}

<ResourceEditorForm
  title="route draft"
  creating={!current}
  {allowed}
  {submission}
>
  <label for="code-slug">Route slug</label><input
    id="code-slug"
    bind:value={slug}
    required
    disabled={!!current}
  />
  <label for="code-pool">Pool</label><select
    id="code-pool"
    bind:value={poolId}
    onchange={() => (researchAccount = '')}
    required
    ><option value="">Choose a pool</option
    >{#each pools as pool (pool.id)}<option value={pool.id}
        >{pool.name} · {pool.kind}</option
      >{/each}
    {#if poolId && !pools.some((p) => p.id === poolId)}
      <option value={poolId}>{poolId} · current selection</option>
    {/if}</select
  >
  <p>
    Saving changes only the draft. Publish to update the immutable route and its
    frozen provider connections. Republish after connection changes or adding an
    account from a new provider.
  </p>
  <div class="form-field">
    <label for="code-max-body">Maximum request body (bytes)</label>
    <input
      id="code-max-body"
      inputmode="numeric"
      bind:value={maxBodyBytes}
      placeholder="Installation limit"
    />
    <small
      >Limits encoded and decoded requests and client WebSocket messages; cannot
      raise installation limits.</small
    >
  </div>
  <label
    ><input
      id="code-paid-participation"
      type="checkbox"
      bind:checked={paidParticipation}
    />Permit paid API targets</label
  >
  <p>
    Mapped models directly use the selected metered API target, even when a
    subscription supports the same model. Fallback targets are separate and keep
    subscription candidates first. Known pricing and every applicable spend
    limit are required on the actual paid attempt.
  </p>
  {#if paidParticipation}
    {@render paidTargetSelectors(
      paidTargets,
      'paid',
      'Paid API',
      'Use the subscription pool',
      (targets) => (paidTargets = targets)
    )}
    <p>
      Subscription-first paid fallback requires bounded recovery, proven
      nonexecution and portable complete input. The exact model is preferred. A
      different model also requires model rewriting and an explicit model pool.
    </p>
    {@render paidTargetSelectors(
      paidFallbackTargets,
      'fallback',
      'Paid fallback',
      'No paid fallback',
      (targets) => (paidFallbackTargets = targets)
    )}
    <small
      >Paid targets use their native HTTP protocol. Publish after changing a
      provider or slot. Targets with request defaults, caller credentials or
      subscription adapters are unsupported.</small
    >
  {/if}
  <label for="code-models">Native models</label><textarea
    id="code-models"
    bind:value={models}
    required
    rows="3"
    placeholder="One native model identifier per line"></textarea>
  <p>
    Model names pass through unchanged. An allowed model is not proof of client
    or inference qualification.
  </p>
  <label class="choice">
    <input
      id="code-model-rewriting"
      type="checkbox"
      bind:checked={modelRewriting}
    />
    Rewrite model identifiers using exact aliases or model pools
  </label>
  <label for="code-aliases">Exact model aliases</label>
  <textarea
    id="code-aliases"
    bind:value={aliases}
    rows="3"
    placeholder="team-code=native-model"></textarea>
  <p>
    Use one alias=native-model per line. Each alias targets one listed native
    model. Publish to make aliases available in discovery and client setup.
    Repointing an alias affects new conversations. Existing conversations keep
    their pinned target while it remains published and authorized.
  </p>
  <label for="code-model-pools">Model recovery pools</label>
  <textarea
    id="code-model-pools"
    bind:value={modelPools}
    rows="3"
    placeholder="team-code=native-model,alternate-model"></textarea>
  <p>
    Use public-model=primary,alternate per line. Start with the public name’s
    normal exact target, followed by distinct listed native models in recovery
    order. Recovery prefers the pinned target. Substitution requires proven
    nonexecution, portable input, an eligible subscription account, current
    authorization and available budgets. Accepted, uncertain, opaque,
    socket-owned, child-bearing and multiple-model state is not replayed across
    models. Paid fallback is separate.
  </p>
  <label for="code-recovery-attempts">Recovery attempts</label>
  <select id="code-recovery-attempts" bind:value={recoveryAttempts}>
    <option value={0}>Disabled</option>
    <option value={1}>One recovery attempt</option>
    <option value={2}>Two recovery attempts</option>
  </select>
  <p>
    Without a configured model pool, recovery keeps the exact model. Zero
    attempts disables automatic recovery.
  </p>
  <label class="choice">
    <input id="code-chat-to-codex" type="checkbox" bind:checked={chatToCodex} />
    Translate Chat through Codex
  </label>
  <p>
    Explicitly enables OpenCode Chat text and function tools through Codex
    Responses after publication. Low, medium and high reasoning effort is
    supported. Signed reasoning, media, server-owned state and other unsupported
    required semantics are refused. Identity compatibility and model rewriting
    require their separate opt-ins. Current OpenCode client and live subscription
    acceptance remain unqualified.
  </p>
  <label class="choice">
    <input
      id="code-messages-to-codex"
      type="checkbox"
      bind:checked={messagesToCodex}
    />
    Translate Messages through Codex
  </label>
  <p>
    Explicitly enables Messages text and tool use through Codex accounts after
    publication. Explicit low, medium and high reasoning effort is supported.
    Signed thinking, numeric reasoning budgets, beta features and other
    unsupported required semantics are refused. Translation alone preserves
    client identity and prompts. Model rewriting and Codex identity
    compatibility require their separate opt-ins.
  </p>
  <label class="choice"
    ><input
      id="code-chat-to-antigravity"
      type="checkbox"
      bind:checked={chatToAntigravity}
    /> Translate Chat through Antigravity</label
  >
  <p>
    Enables the controlled Gemini text and function tool path after publication.
    Signed thought state, reasoning controls, media and server tools are
    refused. This translation preserves identity and prompts. Current models and
    live subscription tool workflows remain unqualified.
  </p>
  <label class="choice">
    <input
      id="code-codex-cli-identity"
      type="checkbox"
      bind:checked={codexCLIIdentity}
    />
    Enable Codex CLI identity compatibility
  </label>
  <p>
    Disabled by default and independent of translation. After publication, Codex
    subscription HTTP and WebSocket requests with no nonblank Originator value
    receive Originator=codex_cli_rs. Explicit Originator values, User-Agent,
    prompts, tools, reasoning and state stay unchanged. No prompt changes are
    enabled. This compatibility identity may violate provider terms and risk
    account suspension. Review the exact change and risk before enabling and
    publishing. Existing route administrators authorize this policy for eligible
    accounts in the route pool.
  </p>
  <label class="choice"
    ><input
      id="code-research-enabled"
      type="checkbox"
      bind:checked={research}
    />Research tools</label
  >
  {#if research}
    <div class="form-field">
      <label for="code-research-account">Research account</label>
      <select id="code-research-account" bind:value={researchAccount} required>
        <option value="">Choose a Z.ai account</option>
        {#each researchAccounts as account (account.id)}
          <option value={account.id}>{account.name}</option>
        {/each}
        {#if researchAccount && !researchAccounts.some((account) => account.id === researchAccount)}
          <option value={researchAccount}>Current account · unavailable</option>
        {/if}
      </select>
      <small
        >Uses an enrolled Z.ai global account in this pool, independently of the
        route's models.</small
      >
      {#if !researchAccounts.length}
        <p>
          Add an eligible Z.ai global account to this pool to enable research.
        </p>
      {/if}
    </div>
    <fieldset>
      <legend>Research services</legend>
      {#each services as service (service.id)}
        <label class="choice">
          <input
            type="checkbox"
            value={service.id}
            bind:group={researchServices}
          />
          {service.label}
        </label>
      {/each}
    </fieldset>
    <p>
      Publish, then copy the client configuration. Your existing OLP key
      authorizes these tools.
    </p>
  {/if}
  <label class="choice"
    ><input type="checkbox" bind:checked={enabled} />Enabled in next publication</label
  >
</ResourceEditorForm>
