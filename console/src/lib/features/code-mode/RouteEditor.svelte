<script lang="ts">
  import { untrack } from 'svelte';
  import {
    saveCodeRoute,
    type CodeAccount,
    type CodePool,
    type CodeRoute,
    type CodeRouteWrite
  } from '$lib/api/code-mode';
  import { nativeModels } from './presentation';

  import {
    useResourceSubmission,
    type ResourceEditorProps
  } from './resourceSubmission.svelte';
  import ResourceEditorForm from './ResourceEditorForm.svelte';

  let {
    current,
    projectId,
    pools,
    accounts,
    allowed,
    onDraftChange,
    onSaved,
    onCancel
  }: ResourceEditorProps<CodeRoute> & {
    pools: CodePool[];
    accounts: CodeAccount[];
  } = $props();
  const initial = untrack(() => current);
  let slug = $state(initial?.slug ?? '');
  let poolId = $state(initial?.pool_id ?? '');
  let maxBodyBytes = $state(initial?.max_body_bytes?.toString() ?? '');
  let models = $state(initial?.models.join('\n') ?? '');
  let enabled = $state(initial?.enabled ?? true);
  let modelRewriting = $state(initial?.model_rewriting ?? false);
  let aliases = $state(
    Object.entries(initial?.aliases ?? {})
      .map(([alias, target]) => `${alias}=${target}`)
      .join('\n')
  );
  let messagesToCodex = $state(initial?.messages_to_codex ?? false);
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
      const aliasEntries = aliases
        .split('\n')
        .map((line) => line.trim())
        .filter(Boolean)
        .map((line) => {
          const [alias, target, ...extra] = line
            .split('=')
            .map((part) => part.trim());
          if (
            !alias ||
            !target ||
            extra.length ||
            !/^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$/.test(alias) ||
            routeModels.includes(alias) ||
            !routeModels.includes(target)
          )
            throw new Error(
              'Use one distinct alias=native-model per line, targeting a listed native model.'
            );
          return [alias, target] as const;
        });
      if (aliasEntries.length && !modelRewriting)
        throw new Error('Enable model rewriting to publish aliases.');
      if (
        aliasEntries.length > 100 ||
        new Set(aliasEntries.map(([alias]) => alias)).size !==
          aliasEntries.length
      )
        throw new Error('Use up to 100 unique aliases.');
      return {
        project_id: projectId,
        slug,
        pool_id: poolId,
        max_body_bytes: maxBodyBytes ? Number(maxBodyBytes) : null,
        models: routeModels,
        aliases: Object.fromEntries(aliasEntries),
        model_rewriting: modelRewriting,
        messages_to_codex: messagesToCodex,
        initial_recovery_attempts: initial?.initial_recovery_attempts ?? 0,
        tools: research
          ? { account_id: researchAccount, services: researchServices }
          : null,
        enabled
      };
    },
    persist: (input, requestId) => saveCodeRoute(input, current, requestId)
  });
</script>

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
    Rewrite model identifiers using exact aliases
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
    unsupported required semantics are refused. Client identity and prompts stay
    unchanged. Model rewriting requires its separate opt-in. Identity and prompt
    compatibility remain disabled.
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
