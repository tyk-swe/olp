<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import ConfirmAction from '$lib/components/ConfirmAction.svelte';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { errorMessage } from '$lib/management/http';
  import { listProjects } from '$lib/features/agents/api';
  import { agentKeys } from '$lib/features/agents/agentKeys';
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
<section class="workflow">
  <a href={resolve('/routes')}>Routes</a>
  <h1 class="page-title">Route templates</h1>
  <button
    class="button button-secondary"
    type="button"
    onclick={() => {
      selected = '';
      results = null;
    }}>New template</button
  >
  {#if templates.error || detail.error}<p role="alert">
      {errorMessage(templates.error ?? detail.error)}
    </p>{/if}
  {#each templates.data ?? [] as value (value.id)}
    <article class="card">
      <h2>{value.name}</h2>
      <p>{value.provider_selector} · {value.model_filter}</p>
      <button
        class="button button-secondary"
        type="button"
        onclick={() => {
          selected = value.id;
          results = null;
        }}>Inspect and edit</button
      >
      {#if access.allows('POST /api/v1/route-templates/{template_id}/apply')}<ConfirmAction
          label="Apply template"
          description={value.auto_publish
            ? 'Apply to matching certified models and publish generated routes immediately'
            : 'Apply to matching certified models and create route drafts'}
          identity={`${value.id}:${value.etag}`}
          run={(requestId) => applyTemplate(value.id, requestId)}
          done={async (result) => {
            results = result.results;
            await Promise.all([
              refresh(),
              client.invalidateQueries({ queryKey: routeKeys.lists })
            ]);
          }}
        />{/if}
      {#if access.allows('DELETE /api/v1/route-templates/{template_id}')}<ConfirmAction
          label="Delete template"
          description={`Delete ${value.name}`}
          identity={`${value.id}:${value.etag}`}
          run={() => deleteTemplate(value)}
          done={async () => {
            if (selected === value.id) selected = '';
            await refresh();
          }}
        />{/if}
    </article>
  {/each}
  {#if current}<h2>Recorded placements</h2>{/if}
  {#if results?.length === 0}<p role="status">
      No matching certified models were placed
    </p>{/if}
  {#each placements as result, index (index)}<p>
      {result.upstream_model} · {result.route_slug} · {result.outcome}{result.reason
        ? ` · ${result.reason}`
        : ''}
    </p>{/each}
  {#if (!selected || current) && access.allows(current ? 'PUT /api/v1/route-templates/{template_id}' : 'POST /api/v1/route-templates')}{#key `${current?.id ?? 'new'}:${editorVersion}`}<TemplateEditor
        {current}
        reload={() => {
          editorVersion += 1;
        }}
        projects={projects.data ?? []}
        {saved}
      />{/key}{/if}
</section>
