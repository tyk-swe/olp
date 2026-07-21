<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { ApiProblem } from '$lib/api/http';
  import {
    diffRouteRevisions,
    listRouteRevisions,
    restoreRouteRevision,
    type RouteRevision,
    type RouteRevisionDiff
  } from '$lib/api/management/routes';

  let { routeId }: { routeId: string } = $props();
  const queryClient = useQueryClient();
  const revisions = createQuery(() => ({
    queryKey: ['route-revisions', routeId],
    queryFn: () => listRouteRevisions(routeId)
  }));

  let busy = $state('');
  let errorMessage = $state('');
  let fromRevision = $state('');
  let toRevision = $state('');
  let revisionDiff = $state<RouteRevisionDiff | null>(null);

  $effect(() => {
    const items = revisions.data ?? [];
    if (items.length && !toRevision) toRevision = items[0].id;
    if (items.length > 1 && !fromRevision) fromRevision = items[1].id;
  });

  function message(error: unknown) {
    return error instanceof ApiProblem
      ? error.problem.detail ?? error.problem.title
      : error instanceof Error ? error.message : 'The control API could not complete the request.';
  }

  async function run(label: string, action: () => Promise<void>) {
    busy = label;
    errorMessage = '';
    try { await action(); } catch (error) { errorMessage = message(error); } finally { busy = ''; }
  }

  async function compareRevisions() {
    if (!fromRevision || !toRevision || fromRevision === toRevision) {
      errorMessage = 'Choose two different revisions to compare.';
      return;
    }
    await run('diff', async () => {
      revisionDiff = await diffRouteRevisions(routeId, fromRevision, toRevision);
    });
  }

  async function restore(revision: RouteRevision) {
    await run(`restore-${revision.id}`, async () => {
      const restored = await restoreRouteRevision(routeId, revision.id);
      await queryClient.invalidateQueries({ queryKey: ['route-drafts'] });
      await queryClient.invalidateQueries({ queryKey: ['route-draft-page'] });
      await goto(resolve(`/routes/${restored.id}`));
    });
  }
</script>

<svelte:head><title>Routes · OpenLLMProxy</title></svelte:head>

<div class="page-header"><div><p class="eyebrow">Gateway · Route history</p><h1 class="page-title">Immutable revisions</h1><p class="page-description">Restoring history creates a new editable draft. It never rolls back keys or provider credentials.</p></div><a class="button button-secondary" href={resolve('/routes')}>All routes</a></div>
{#if errorMessage}<div class="inline-problem" role="alert">{errorMessage}</div>{/if}
{#if revisions.isPending}
  <div class="loading-state" role="status">Loading revision history…</div>
{:else if revisions.isError}
  <div class="inline-problem" role="alert">{message(revisions.error)} <button class="button button-secondary" type="button" onclick={() => revisions.refetch()}>Retry</button></div>
{:else if !revisions.data?.length}
  <section class="card empty-state"><div><h2>No activated revisions</h2><p>Activate a validated route draft to create revision 1.</p></div></section>
{:else}
  <section class="card revision-compare" aria-labelledby="compare-heading"><div><p class="eyebrow">Revision diff</p><h2 id="compare-heading">Compare configuration</h2></div><label>From<select bind:value={fromRevision}>{#each revisions.data as revision (revision.id)}<option value={revision.id}>Revision {revision.revision}</option>{/each}</select></label><label>To<select bind:value={toRevision}>{#each revisions.data as revision (revision.id)}<option value={revision.id}>Revision {revision.revision}</option>{/each}</select></label><button class="button button-secondary" type="button" onclick={compareRevisions} disabled={busy === 'diff'}>{busy === 'diff' ? 'Comparing…' : 'Compare'}</button></section>
  {#if revisionDiff}
    <section class="diff-grid" aria-label="Revision differences">
      <article class="card"><p>Route settings</p><strong>{[revisionDiff.slug_changed && 'slug', revisionDiff.timeout_changed && 'deadline', revisionDiff.max_attempts_changed && 'attempts'].filter(Boolean).join(', ') || 'unchanged'}</strong></article>
      <article class="card"><p>Operations added</p>{#if revisionDiff.operations_added.length}<ul>{#each revisionDiff.operations_added as item (item)}<li><code>{item}</code></li>{/each}</ul>{:else}<strong>None</strong>{/if}<p>Operations removed</p>{#if revisionDiff.operations_removed.length}<ul>{#each revisionDiff.operations_removed as item (item)}<li><code>{item}</code></li>{/each}</ul>{:else}<strong>None</strong>{/if}</article>
      <article class="card"><p>Target changes</p>{#if revisionDiff.targets_added.length}<strong>Added</strong><ul>{#each revisionDiff.targets_added as item (item)}<li><code>{item}</code></li>{/each}</ul>{/if}{#if revisionDiff.targets_removed.length}<strong>Removed</strong><ul>{#each revisionDiff.targets_removed as item (item)}<li><code>{item}</code></li>{/each}</ul>{/if}{#if revisionDiff.targets_changed.length}<strong>Changed</strong><ul>{#each revisionDiff.targets_changed as item (item)}<li><code>{item}</code></li>{/each}</ul>{/if}{#if !revisionDiff.targets_added.length && !revisionDiff.targets_removed.length && !revisionDiff.targets_changed.length}<strong>None</strong>{/if}</article>
    </section>
  {/if}
  <div class="table-shell revision-table-shell"><table class="data-table revision-table"><thead><tr><th>Revision</th><th>Activated</th><th>Operations</th><th>Deadline / attempts</th><th>Targets</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each revisions.data as revision (revision.id)}<tr><td data-label="Revision"><strong>Revision {revision.revision}</strong><br /><code>{revision.id}</code></td><td data-label="Activated">{new Date(revision.activated_at).toLocaleString()}</td><td data-label="Operations">{revision.operations.join(', ')}</td><td data-label="Deadline / attempts">{revision.overall_timeout_ms.toLocaleString()} ms / {revision.max_attempts}</td><td data-label="Targets">{revision.targets.length}</td><td class="revision-action"><button class="button button-secondary" type="button" onclick={() => restore(revision)} disabled={Boolean(busy)}>{busy === `restore-${revision.id}` ? 'Restoring…' : 'Restore as draft'}</button></td></tr>{/each}</tbody></table></div>
{/if}

<style>
  h2 { margin: 0 0 .75rem; font-size: 1.15rem; letter-spacing: -.025em; }
  code { font: .7rem 'JetBrains Mono Variable', monospace; }
  .revision-compare { display: flex; align-items: end; gap: .75rem; margin: 1.5rem 0 1rem; padding: clamp(1.1rem, 2.5vw, 1.5rem); }
  .revision-compare > div { margin-right: auto; }
  .revision-compare h2 { margin: 0; }
  .revision-compare label { display: grid; gap: .3rem; font-size: .72rem; font-weight: 700; }
  .revision-compare select { min-height: 2.5rem; padding: .5rem .65rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  .diff-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: .75rem; margin-bottom: 1rem; }
  .diff-grid article { min-width: 0; padding: 1rem; }
  .diff-grid p { margin: 0; color: var(--foreground-muted); font-size: .75rem; }
  .diff-grid p:not(:first-child) { margin-top: .8rem; }
  .diff-grid strong { display: block; margin-top: .25rem; }
  .diff-grid ul { margin: .3rem 0 0; padding-left: 1rem; }
  .diff-grid li { overflow-wrap: anywhere; }
  @media (max-width: 48rem) {
    .revision-compare { display: grid; }
    .diff-grid { grid-template-columns: 1fr; }
    .revision-table-shell { overflow: visible; border: 0; background: transparent; box-shadow: none; }
    .revision-table, .revision-table tbody { display: grid; gap: .75rem; }
    .revision-table thead { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0, 0, 0, 0); white-space: nowrap; }
    .revision-table tbody tr { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: .75rem; padding: 1rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface); box-shadow: var(--shadow-sm); }
    .revision-table tbody td { display: block; min-width: 0; min-height: 0; padding: 0; border: 0; overflow-wrap: anywhere; }
    .revision-table tbody td::before { display: block; margin-bottom: .2rem; color: var(--foreground-muted); content: attr(data-label); font-size: .68rem; font-weight: 760; letter-spacing: .045em; text-transform: uppercase; }
    .revision-table tbody td:first-child, .revision-table tbody .revision-action { grid-column: 1 / -1; }
    .revision-table tbody .revision-action::before { display: none; }
    .revision-table tbody .revision-action .button { width: 100%; }
  }
</style>
