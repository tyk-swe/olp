<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { confirmDiscardChanges } from '#lib/editing/unsavedChanges.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { stateLabel } from '#lib/format.js';
  import { errorMessage } from '#lib/management/http.js';
  import { listProjects } from '#lib/features/agents/api.js';
  import { agentKeys } from '#lib/features/agents/agentKeys.js';
  import {
    listTemplates,
    getTemplate,
    deleteTemplate,
    applyTemplate,
    type RouteTemplate
  } from './api';
  import { routeKeys } from './routeKeys';
  import TemplateEditor from './TemplateEditor.svelte';

  const access = useRole();
  const client = useQueryClient();
  const templates = createQuery(() => ({
    queryKey: routeKeys.templates(),
    queryFn: listTemplates
  }));
  const projects = createQuery(() => ({
    queryKey: agentKeys.projects(),
    queryFn: listProjects
  }));
  let selected = $state('');
  let editorVersion = $state(0);
  let editor = $state<ReturnType<typeof TemplateEditor>>();
  let results = $state<
    Awaited<ReturnType<typeof applyTemplate>>['results'] | null
  >(null);
  const detail = createQuery(() => ({
    queryKey: routeKeys.template(selected),
    queryFn: () => getTemplate(selected),
    enabled: Boolean(selected)
  }));
  const current = $derived(detail.data ?? null);
  const placements = $derived(results ?? current?.routes ?? []);
  const canEdit = $derived(
    access.allows('PUT /api/v1/route-templates/{template_id}')
  );

  function select(id: string) {
    if (!confirmDiscardChanges(editor?.isDirty() ?? false)) return;
    selected = id;
    editorVersion += 1;
    results = null;
  }
  async function saved(value: RouteTemplate) {
    client.setQueryData(routeKeys.template(value.id), value);
    selected = value.id;
    await refresh();
  }
  async function refresh() {
    await client.invalidateQueries({ queryKey: routeKeys.templates() });
  }
</script>

<svelte:head><title>Route templates · OpenLLMProxy</title></svelte:head>
<a class="text-button" href={resolve('routes')}>All routes</a>
<div class="toolbar">
  <div class="page-actions">
    {#if access.allows('POST /api/v1/route-templates')}
      <button
        class="button button-primary"
        type="button"
        onclick={() => select('')}>New template</button
      >
    {/if}
  </div>
</div>
{#if detail.error}
  <p class="inline-problem" role="alert">{errorMessage(detail.error)}</p>
{/if}
{#if templates.error}
  <p class="inline-problem" role="alert">{errorMessage(templates.error)}</p>
{:else if templates.isPending}
  <div class="loading-state" role="status">Loading templates…</div>
{:else if !templates.data?.length}
  <div class="card empty-state">No route templates yet.</div>
{:else}
  <ScrollTable label="Route templates">
    <table class="data-table">
      <caption class="sr-only">Route templates</caption>
      <thead>
        <tr>
          <th scope="col">Template</th>
          <th scope="col">Provider selector</th>
          <th scope="col">Model filter</th>
          <th scope="col">Actions</th>
        </tr>
      </thead>
      <tbody>
        {#each templates.data as value (value.id)}
          <tr>
            <td><strong>{value.name}</strong></td>
            <td><code>{value.provider_selector}</code></td>
            <td><code>{value.model_filter}</code></td>
            <td>
              <div class="page-actions">
                <button
                  class="button button-secondary"
                  type="button"
                  onclick={() => select(value.id)}
                  >{canEdit ? 'Inspect and edit' : 'Inspect'}</button
                >
                {#if access.allows('POST /api/v1/route-templates/{template_id}/apply')}
                  <ConfirmAction
                    label="Apply template"
                    description={value.auto_publish
                      ? 'Apply to matching certified models and publish generated routes immediately.'
                      : 'Apply to matching certified models and create route drafts.'}
                    identity={`${value.id}:${value.etag}`}
                    run={(requestId) => applyTemplate(value.id, requestId)}
                    done={async (result) => {
                      results = result.results;
                      await Promise.all([
                        refresh(),
                        client.invalidateQueries({ queryKey: routeKeys.lists })
                      ]);
                    }}
                  />
                {/if}
                {#if access.allows('DELETE /api/v1/route-templates/{template_id}')}
                  <ConfirmAction
                    danger
                    label="Delete template"
                    description={`Delete ${value.name}.`}
                    identity={`${value.id}:${value.etag}`}
                    run={() => deleteTemplate(value)}
                    done={async () => {
                      if (selected === value.id) selected = '';
                      await refresh();
                    }}
                  />
                {/if}
              </div>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}

{#if current || results}
  <section class="section" aria-labelledby="placements-heading">
    <h2 id="placements-heading">Recorded placements</h2>
    {#if !placements.length}
      <p class="inline-status" role="status">
        No matching certified models were placed
      </p>
    {:else}
      <ScrollTable label="Recorded placements">
        <table class="data-table">
          <caption class="sr-only">Template placements by model</caption>
          <thead>
            <tr>
              <th scope="col">Model</th>
              <th scope="col">Route</th>
              <th scope="col">Outcome</th>
              <th scope="col">Reason</th>
            </tr>
          </thead>
          <tbody>
            {#each placements as result (`${result.provider_model_id}:${result.route_slug}`)}
              <tr>
                <td>{result.upstream_model}</td>
                <td><code>{result.route_slug}</code></td>
                <td><span class="badge">{stateLabel(result.outcome)}</span></td>
                <td>{result.reason ?? ''}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    {/if}
  </section>
{/if}

{#if (!selected || current) && access.allows(current ? 'PUT /api/v1/route-templates/{template_id}' : 'POST /api/v1/route-templates')}
  {#key `${current?.id ?? 'new'}:${editorVersion}`}
    <TemplateEditor
      bind:this={editor}
      {current}
      reload={() => {
        editorVersion += 1;
      }}
      projects={projects.data ?? []}
      {saved}
    />
  {/key}
{/if}
