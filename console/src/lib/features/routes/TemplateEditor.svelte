<script lang="ts">
  import { untrack } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { guardUnsavedChanges } from '#lib/editing/unsavedChanges.js';
  import ConflictNotice from '#lib/shell/ConflictNotice.svelte';
  import { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import { parseJsonObject } from '#lib/editing/json.js';
  import type { RoutingPolicy } from './api';
  import { fidelityOptions, type FidelityMode } from './routeFidelity';
  import { saveTemplate, type RouteTemplate } from './api';
  import { routeKeys } from './routeKeys';
  let {
    current,
    projects,
    saved,
    reload
  }: {
    current: RouteTemplate | null;
    projects: { id: string; name: string }[];
    reload?: () => void;
    saved: (value: RouteTemplate) => Promise<void>;
  } = $props();
  const initial = untrack(() => current);
  let baseline = $state(initial);
  let name = $state(initial?.name ?? '');
  let project = $state(initial?.project_id ?? '');
  let selector = $state(initial?.provider_selector ?? '');
  let filter = $state(initial?.model_filter ?? '*');
  let slug = $state(initial?.slug_pattern ?? '{model}');
  let timeout = $state(initial?.overall_timeout_ms ?? 60000);
  let attempts = $state(initial?.max_attempts ?? 2);
  let fidelity = $state<FidelityMode>(initial?.fidelity.mode ?? 'strict');
  let policy = $state(
    initial?.routing_policy
      ? JSON.stringify(initial.routing_policy, null, 2)
      : ''
  );
  let publish = $state(initial?.auto_publish ?? false);
  const client = useQueryClient();
  const action = useAction(() =>
    client.invalidateQueries({ queryKey: routeKeys.templates() })
  );
  const intent = useMutationIntent();
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const parsed = policy.trim() ? parseJsonObject(policy) : null;
    if (policy.trim() && !parsed) {
      action.error = 'Routing policy must be a JSON object';
      return;
    }
    const body = {
      name,
      project_id: project || null,
      provider_selector: selector,
      model_filter: filter,
      slug_pattern: slug,
      overall_timeout_ms: timeout,
      max_attempts: attempts,
      fidelity: { mode: fidelity },
      routing_policy: parsed as RoutingPolicy | null,
      auto_publish: publish
    };
    await action.run(
      () =>
        intent.submit({ current: baseline, body }, (input, requestId) =>
          saveTemplate(input.current, input.body, requestId)
        ),
      async (value) => {
        baseline = value;
        clean = draft();
        await saved(value);
      }
    );
  }
  const draft = () =>
    JSON.stringify([
      name,
      project,
      selector,
      filter,
      slug,
      timeout,
      attempts,
      fidelity,
      policy,
      publish
    ]);
  let clean = $state(draft());
  export function isDirty() {
    return draft() !== clean;
  }
  guardUnsavedChanges(isDirty);
</script>

<form class="card form-card" onsubmit={submit}>
  <h2>{current ? 'Edit template' : 'Create template'}</h2>
  <label class="form-field"
    >Name<input
      required
      pattern={'[a-z0-9][a-z0-9\\-]{0,62}'}
      bind:value={name}
    /></label
  >
  <label class="form-field"
    >Project<select disabled={Boolean(baseline)} bind:value={project}
      ><option value="">Installation</option
      >{#each projects as item (item.id)}<option value={item.id}
          >{item.name}</option
        >{/each}</select
    ></label
  >
  <label class="form-field"
    >Provider selector<input
      required
      placeholder="provider UUID or vendor ID, prefixed with provider: or vendor:"
      bind:value={selector}
    /></label
  >
  <label class="form-field"
    >Model filter<input required bind:value={filter} /></label
  >
  <label class="form-field"
    >Route slug pattern<input required bind:value={slug} /></label
  >
  <p class="inline-status">
    Use {'{model}'} and optionally {'{vendor}'} in the slug pattern
  </p>
  <label class="form-field"
    >Timeout in milliseconds<input
      type="number"
      min="1"
      max="3600000"
      required
      bind:value={timeout}
    /></label
  >
  <label class="form-field"
    >Maximum attempts<input
      type="number"
      min="1"
      max="32767"
      required
      bind:value={attempts}
    /></label
  >
  <label class="form-field"
    >Fidelity<select bind:value={fidelity}
      >{#each fidelityOptions as option (option.value)}<option
          value={option.value}>{option.label}</option
        >{/each}</select
    ></label
  >
  <label class="form-field full"
    >Routing policy JSON<textarea
      bind:value={policy}
      placeholder="Leave blank to inherit"></textarea></label
  >
  <label class="check-field"
    ><input type="checkbox" bind:checked={publish} />Automatically publish
    generated routes</label
  >
  {#if publish}<p class="inline-status">
      Applying this template or activating a matching provider can publish
      routes immediately
    </p>{/if}
  <div class="page-actions">
    <button class="button button-primary" disabled={action.busy}
      >Save template</button
    >
  </div>
  {#if reload && current?.etag !== baseline?.etag}
    <ConflictNotice notice="newer" onReload={reload} disabled={action.busy} />
  {/if}
  {#if action.error}<p class="inline-problem" role="alert">
      {action.error}
    </p>{/if}
</form>
