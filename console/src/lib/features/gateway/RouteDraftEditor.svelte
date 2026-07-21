<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { ApiProblem } from '$lib/api/http';
  import {
    activateRoute,
    createRouteDraft,
    deleteRouteDraft,
    getRouteDraft,
    replaceRouteDraft,
    simulateRoute,
    validateRoute,
    type RouteActivation,
    type RouteDraft,
    type RouteSimulation
  } from '$lib/api/management/routes';
  import { listProviderModelInventory } from '$lib/api/management/providers';
  import {
    buildCreateRouteDraftInput,
    buildReplaceRouteDraftInput,
    eligibleTargetTuples,
    missingTargetOperations,
    modesFor,
    operationOptions,
    routeEligibilityWarnings as findRouteEligibilityWarnings,
    surfacesFor,
    toRouteModelOptions,
    validateRouteEditor,
    type EditableTarget
  } from './routeEditor';

  let { routeId }: { routeId?: string } = $props();
  const resourceId = $derived(routeId ?? '');
  const isNew = $derived(!resourceId);
  const queryClient = useQueryClient();

  const draft = createQuery(() => ({
    queryKey: ['route-draft', resourceId],
    queryFn: () => getRouteDraft(resourceId),
    enabled: Boolean(resourceId)
  }));
  const providerModels = createQuery(() => ({
    queryKey: ['enabled-provider-models'],
    queryFn: () => listProviderModelInventory(true)
  }));
  const modelOptions = $derived(toRouteModelOptions(providerModels.data ?? []));

  let slug = $state('default');
  let operations = $state<string[]>(['generation']);
  let overallTimeoutMs = $state(120000);
  let maxAttempts = $state(2);
  let targets = $state<EditableTarget[]>([]);
  let initialized = $state('');
  let busy = $state('');
  let errorMessage = $state('');
  let notice = $state('');
  let seed = $state('setup-preview');
  let simulationOperation = $state('generation');
  let simulationSurface = $state('openai');
  let simulationMode = $state('streaming');
  let simulation = $state<RouteSimulation | null>(null);
  let activation = $state<RouteActivation | null>(null);
  let validated = $state(false);
  const editorValues = $derived({ slug, operations, overallTimeoutMs, maxAttempts, targets });

  $effect(() => {
    const current = draft.data;
    if (!current || initialized === current.etag) return;
    initialized = current.etag;
    slug = current.slug;
    operations = [...current.operations];
    overallTimeoutMs = current.overall_timeout_ms;
    maxAttempts = current.max_attempts;
    targets = current.targets.map((target) => ({
      providerModelId: target.provider_model_id,
      priority: target.priority,
      weight: target.weight,
      timeoutMs: target.timeout_ms
    }));
    validated = current.state === 'validated';
  });

  $effect(() => {
    if (!operations.includes(simulationOperation)) {
      simulationOperation = operations[0] ?? 'generation';
    }
    const surfaces = surfacesFor(simulationOperation);
    if (!surfaces.includes(simulationSurface)) simulationSurface = surfaces[0] ?? 'openai';
    const modes = modesFor(simulationOperation);
    if (!modes.includes(simulationMode)) simulationMode = modes[0] ?? 'unary';
  });

  const routeEligibilityWarnings = $derived(
    findRouteEligibilityWarnings(targets, modelOptions, operations)
  );

  function message(error: unknown) {
    return error instanceof ApiProblem
      ? error.problem.detail ?? error.problem.title
      : error instanceof Error ? error.message : 'The control API could not complete the request.';
  }

  async function run(label: string, action: () => Promise<void>) {
    busy = label;
    errorMessage = '';
    notice = '';
    try { await action(); } catch (error) { errorMessage = message(error); } finally { busy = ''; }
  }

  function toggleOperation(operation: string, checked: boolean) {
    operations = checked
      ? [...new Set([...operations, operation])]
      : operations.filter((item) => item !== operation);
    validated = false;
  }

  function addTarget() {
    const firstUnused = modelOptions.find((option) => !targets.some((target) => target.providerModelId === option.id)) ?? modelOptions[0];
    if (!firstUnused) return;
    targets = [...targets, { providerModelId: firstUnused.id, priority: 1, weight: 100, timeoutMs: 60000 }];
    validated = false;
  }

  function removeTarget(index: number) {
    targets = targets.filter((_, targetIndex) => targetIndex !== index);
    validated = false;
  }

  async function create(event: SubmitEvent) {
    event.preventDefault();
    const issue = validateRouteEditor(editorValues);
    if (issue) { errorMessage = issue; return; }
    await run('save', async () => {
      const id = await createRouteDraft(buildCreateRouteDraftInput(editorValues, modelOptions));
      await queryClient.invalidateQueries({ queryKey: ['route-drafts'] });
      await queryClient.invalidateQueries({ queryKey: ['route-draft-page'] });
      await goto(resolve(`/routes/${id}`));
    });
  }

  async function save(current: RouteDraft) {
    const issue = validateRouteEditor(editorValues);
    if (issue) { errorMessage = issue; return; }
    await run('save', async () => {
      const updated = await replaceRouteDraft(
        current.id,
        current.etag,
        buildReplaceRouteDraftInput(editorValues)
      );
      queryClient.setQueryData(['route-draft', current.id], updated);
      validated = false;
      notice = 'Draft saved. Simulate and validate before activation.';
    });
  }

  async function simulate(current: RouteDraft) {
    await run('simulate', async () => {
      simulation = await simulateRoute(current.id, {
        operation: simulationOperation,
        surface: simulationSurface,
        mode: simulationMode,
        seed: seed || 'preview'
      });
      notice = 'Deterministic attempt order calculated from the saved draft.';
    });
  }

  async function validate(current: RouteDraft) {
    await run('validate', async () => {
      const validation = await validateRoute(current);
      queryClient.setQueryData<RouteDraft>(['route-draft', current.id], {
        ...current,
        state: validation.state,
        etag: validation.etag
      });
      validated = true;
      notice = 'Validation passed. The saved draft is ready to activate.';
    });
  }

  async function activate(current: RouteDraft) {
    await run('activate', async () => {
      activation = await activateRoute(current);
      notice = `Route activated as revision ${activation.revision} in runtime generation ${activation.runtime_generation.sequence}.`;
      await Promise.all([
        draft.refetch(),
        queryClient.invalidateQueries({ queryKey: ['route-drafts'] }),
        queryClient.invalidateQueries({ queryKey: ['route-draft-page'] }),
        queryClient.invalidateQueries({ queryKey: ['routes'] }),
        queryClient.invalidateQueries({ queryKey: ['route-page'] })
      ]);
    });
  }

  async function remove(current: RouteDraft) {
    if (!confirm(`Delete draft “${current.slug}”?`)) return;
    await run('delete', async () => {
      await deleteRouteDraft(current.id, current.etag);
      await queryClient.invalidateQueries({ queryKey: ['route-drafts'] });
      await queryClient.invalidateQueries({ queryKey: ['route-draft-page'] });
      await goto(resolve('/routes'));
    });
  }
</script>

<svelte:head><title>Routes · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div><p class="eyebrow">Gateway · Route Studio</p><h1 class="page-title">{isNew ? 'Build a route draft.' : (draft.data?.slug ?? 'Route draft')}</h1><p class="page-description">Set explicit eligibility, deterministic priority and weight, and bounded failover before publishing.</p></div>
  <div class="page-actions"><a class="button button-secondary" href={resolve('/routes')}>Cancel</a>{#if resourceId && draft.data}<button class="button button-secondary danger-button" type="button" onclick={() => remove(draft.data!)} disabled={Boolean(busy)}>Delete draft</button>{/if}</div>
</div>
{#if errorMessage}<div class="inline-problem" role="alert">{errorMessage}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}
{#if (!isNew && draft.isPending) || providerModels.isPending}
  <div class="loading-state" role="status">Loading Route Studio…</div>
{:else if (!isNew && draft.isError) || providerModels.isError}
  <div class="inline-problem" role="alert">{message(draft.error ?? providerModels.error)} <button class="button button-secondary" type="button" onclick={() => { draft.refetch(); providerModels.refetch(); }}>Retry</button></div>
{:else}
  <form class="studio" onsubmit={isNew ? create : (event) => { event.preventDefault(); if (draft.data) save(draft.data); }}>
    <div class="studio-main">
      <section class="card editor" aria-labelledby="route-contract-heading"><p class="eyebrow">Public contract</p><h2 id="route-contract-heading">Slug and operations</h2><div class="form-grid"><div class="form-field full"><label for="route-slug">Public model slug</label><input id="route-slug" autocomplete="off" bind:value={slug} oninput={() => validated = false} /><small>Clients send this value as their model. Direct provider/model addressing is unavailable.</small></div><fieldset class="form-field full operations"><legend>Supported operations</legend>{#each operationOptions as option (option[0])}<label><input type="checkbox" checked={operations.includes(option[0])} onchange={(event) => toggleOperation(option[0], event.currentTarget.checked)} /> {option[1]}</label>{/each}</fieldset></div></section>
      <section class="card editor" aria-labelledby="targets-heading"><div class="section-heading"><div><p class="eyebrow">Attempt order</p><h2 id="targets-heading">Eligible targets</h2></div><button class="button button-secondary" type="button" onclick={addTarget} disabled={!modelOptions.length}>Add target</button></div>
        {#if !modelOptions.length}<div class="empty-state compact"><p>No enabled models are available. <a href={resolve('/models')}>Review model eligibility</a>.</p></div>{/if}
        <ol class="targets">
          {#each targets as target, index (index)}
            <li>
              <span class="target-number" aria-hidden="true">{index + 1}</span>
              <div class="target-fields">
                <div class="form-field model-select"><label for={`target-model-${index}`}>Provider model</label><select id={`target-model-${index}`} bind:value={target.providerModelId} onchange={() => validated = false}>{#each modelOptions as option (option.id)}<option value={option.id}>{option.label}</option>{/each}</select></div>
                <div class="form-field"><label for={`priority-${index}`}>Priority</label><input id={`priority-${index}`} type="number" min="1" max="100" bind:value={target.priority} oninput={() => validated = false} /></div>
                <div class="form-field"><label for={`weight-${index}`}>Weight</label><input id={`weight-${index}`} type="number" min="1" max="10000" bind:value={target.weight} oninput={() => validated = false} /></div>
                <div class="form-field"><label for={`timeout-${index}`}>Attempt timeout (ms)</label><input id={`timeout-${index}`} type="number" min="100" bind:value={target.timeoutMs} oninput={() => validated = false} /></div>
              </div>
              <button class="remove-target" type="button" aria-label={`Remove target ${index + 1}`} onclick={() => removeTarget(index)}>×</button>
              <div class:warning={missingTargetOperations(target, modelOptions, operations).length > 0} class="target-eligibility">
                {#if eligibleTargetTuples(target, modelOptions, operations).length}
                  <span><strong>Certified tuples:</strong> {eligibleTargetTuples(target, modelOptions, operations).join(', ')}</span>
                {:else}
                  <span>No selected operation has a certified tuple on this target.</span>
                {/if}
                {#if missingTargetOperations(target, modelOptions, operations).length}<span><strong>Missing:</strong> {missingTargetOperations(target, modelOptions, operations).join(', ')}</span>{/if}
              </div>
            </li>
          {/each}
        </ol>
        {#if routeEligibilityWarnings.length}<div class="eligibility-warning" role="status"><strong>Route eligibility is incomplete.</strong><span>No selected target has a certified tuple for: {routeEligibilityWarnings.join(', ')}.</span></div>{/if}
      </section>
      <section class="card editor advanced" aria-labelledby="advanced-heading"><p class="eyebrow">Advanced</p><h2 id="advanced-heading">Deadline and failover</h2><div class="form-grid"><div class="form-field"><label for="overall-timeout">Overall deadline (ms)</label><input id="overall-timeout" type="number" min="100" bind:value={overallTimeoutMs} oninput={() => validated = false} /></div><div class="form-field"><label for="max-attempts">Maximum attempts</label><input id="max-attempts" type="number" min="1" bind:value={maxAttempts} oninput={() => validated = false} /></div></div><details><summary>Exactly when will OLP try another target?</summary><p>Only before response bytes are committed, and only for connection/transport failures, configured timeouts, HTTP 429, or HTTP 5xx. There are no hidden SDK retries, hedges, nested routes, or retries after bytes reach the client. Weighted rendezvous ordering is deterministic inside each priority group.</p></details></section>
    </div>
    <aside class="card publish-panel" aria-labelledby="publish-heading"><p class="eyebrow">Draft controls</p><h2 id="publish-heading">Test before activation</h2><p>Saving changes invalidates prior validation.</p><button class="button button-secondary" type="submit" disabled={Boolean(busy)}>{busy === 'save' ? 'Saving…' : isNew ? 'Create draft' : 'Save draft'}</button>
      {#if !isNew && draft.data}
        <hr />
        <label for="simulation-operation">Dry-run operation</label>
        <select id="simulation-operation" bind:value={simulationOperation}>{#each operations as operation (operation)}<option value={operation}>{operation}</option>{/each}</select>
        <label for="simulation-surface">Client surface</label>
        <select id="simulation-surface" bind:value={simulationSurface}>{#each surfacesFor(simulationOperation) as surface (surface)}<option value={surface}>{surface}</option>{/each}</select>
        <label for="simulation-mode">Transport mode</label>
        <select id="simulation-mode" bind:value={simulationMode}>{#each modesFor(simulationOperation) as mode (mode)}<option value={mode}>{mode}</option>{/each}</select>
        <label for="simulation-seed">Dry-run seed</label>
        <input id="simulation-seed" bind:value={seed} />
        <button class="button button-secondary" type="button" onclick={() => simulate(draft.data!)} disabled={Boolean(busy)}>{busy === 'simulate' ? 'Simulating…' : 'Simulate order'}</button>
        <button class="button button-secondary" type="button" onclick={() => validate(draft.data!)} disabled={Boolean(busy)}>{busy === 'validate' ? 'Validating…' : 'Validate draft'}</button>
        <button class="button button-primary" type="button" onclick={() => activate(draft.data!)} disabled={Boolean(busy) || !validated}>{busy === 'activate' ? 'Activating…' : 'Activate route'}</button>
      {/if}
      {#if activation}<div class="activation"><strong>Revision {activation.revision} active</strong><span>Runtime generation {activation.runtime_generation.sequence}</span><a href={resolve(`/routes/${activation.route_id}/revisions`)}>View revision history</a></div>{/if}
    </aside>
  </form>
  {#if simulation}<section class="card simulation" aria-labelledby="simulation-heading"><div class="section-heading"><div><p class="eyebrow">Deterministic dry run</p><h2 id="simulation-heading">Attempt explanation</h2></div><code>seed: {simulation.deterministic_seed}</code></div><ol>{#each simulation.targets as target (target.target_id)}<li class:ineligible={!target.eligible}><span class="attempt">{target.attempt ?? '—'}</span><div><strong>{target.provider_name} · {target.provider_model}</strong><p>{target.eligible ? `Eligible in priority group ${target.priority}` : target.reason ?? 'Capability tuple is not eligible'}</p></div><span class:success={target.eligible} class:warning={!target.eligible} class="badge">{target.eligible ? 'eligible' : 'filtered'}</span></li>{/each}</ol></section>{/if}
{/if}

<style>
  h2 { margin: 0 0 .75rem; font-size: 1.15rem; letter-spacing: -.025em; }
  .success-banner { margin: 1rem 0; padding: .85rem 1rem; border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border)); border-radius: .375rem; background: var(--success-soft); color: var(--success); }
  .studio { display: grid; grid-template-columns: minmax(0, 1fr) 19rem; gap: 1rem; margin-top: 1.4rem; align-items: start; }
  .studio-main { display: grid; gap: 1rem; min-width: 0; }
  .editor, .publish-panel, .simulation { padding: clamp(1.1rem, 2.5vw, 1.5rem); }
  .operations { display: grid; grid-template-columns: repeat(3, 1fr); }
  .operations legend { margin-bottom: .4rem; font-weight: 700; }
  .operations label { display: flex; min-height: 2.75rem; align-items: center; gap: .45rem; font-weight: 600; }
  .section-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; }
  .targets { display: grid; gap: .65rem; margin: 1rem 0 0; padding: 0; list-style: none; }
  .targets li { display: grid; grid-template-columns: 2rem minmax(0, 1fr) 2.75rem; gap: .65rem; align-items: end; padding: .75rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface-subtle); }
  .target-number { display: grid; width: 2rem; height: 2rem; place-items: center; margin-bottom: .35rem; border-radius: 50%; background: var(--accent-soft); color: var(--accent-strong); font: 750 .72rem 'JetBrains Mono Variable'; }
  .target-fields { display: grid; grid-column: 2; grid-template-columns: minmax(12rem, 2fr) repeat(3, minmax(7rem, 1fr)); gap: .6rem; }
  .remove-target { grid-column: 3; grid-row: 1; width: 2.5rem; height: 2.5rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface); color: var(--danger); font-size: 1.3rem; }
  .target-eligibility { display: grid; grid-column: 2 / -1; gap: .2rem; color: var(--success); font-size: .72rem; }
  .target-eligibility.warning { color: var(--warning); }
  .eligibility-warning { display: grid; gap: .2rem; margin-top: .75rem; padding: .75rem; border: 1px solid color-mix(in srgb, var(--warning) 45%, var(--border)); border-radius: .375rem; background: var(--warning-soft); color: var(--warning); font-size: .78rem; }
  .advanced details { margin-top: 1rem; padding: .8rem; border: 1px solid var(--border); border-radius: .375rem; }
  .advanced summary { min-height: 2.75rem; font-weight: 700; }
  .advanced details p, .publish-panel p { color: var(--foreground-muted); }
  .publish-panel { position: sticky; top: 5rem; display: grid; gap: .65rem; }
  .publish-panel h2, .publish-panel p { margin-bottom: 0; }
  .publish-panel hr { width: 100%; margin: .5rem 0; border: 0; border-top: 1px solid var(--border); }
  .publish-panel > :is(input, select) { min-height: 2.5rem; padding: .5rem .65rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  .activation { display: grid; gap: .2rem; padding: .75rem; border-radius: .375rem; background: var(--success-soft); color: var(--success); font-size: .78rem; }
  .activation a { min-height: 2.75rem; padding-top: .65rem; font-weight: 750; }
  .simulation { margin-top: 1rem; }
  .simulation ol { margin: 1rem 0 0; padding: 0; list-style: none; }
  .simulation li { display: grid; grid-template-columns: 2.2rem 1fr auto; align-items: center; gap: .75rem; min-height: 4rem; border-top: 1px solid var(--border); }
  .simulation li.ineligible { color: var(--foreground-muted); }
  .simulation p { margin: .15rem 0 0; color: var(--foreground-muted); font-size: .78rem; }
  .attempt { display: grid; width: 2rem; height: 2rem; place-items: center; border-radius: 50%; background: var(--surface-subtle); font: 700 .72rem 'JetBrains Mono Variable'; }
  .compact { min-height: 6rem; }
  .compact a { color: var(--accent-strong); font-weight: 700; }
  .danger-button { color: var(--danger); }
  code { font: .7rem 'JetBrains Mono Variable', monospace; }
  @media (max-width: 76rem) { .studio { grid-template-columns: 1fr; } .publish-panel { position: static; grid-template-columns: repeat(3, 1fr); } .publish-panel > :is(.eyebrow, h2, p, hr, label, input, select, .activation) { grid-column: 1 / -1; } .target-fields { grid-template-columns: repeat(3, 1fr); } .model-select { grid-column: 1 / -1; } }
  @media (max-width: 48rem) { .operations { grid-template-columns: 1fr; } .targets li { grid-template-columns: 1fr 2.75rem; } .target-number { display: none; } .target-fields { grid-column: 1; grid-template-columns: 1fr; } .remove-target { grid-column: 2; } .target-eligibility { grid-column: 1 / -1; } .model-select { grid-column: auto; } .publish-panel { grid-template-columns: 1fr; } }
</style>
