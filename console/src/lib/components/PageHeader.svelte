<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    title,
    description,
    meta,
    actions
  }: {
    title: string;
    description?: string;
    meta?: Snippet;
    actions?: Snippet;
  } = $props();
</script>

<header class="page-head">
  <div class="heading">
    <div class="title-row">
      <h1 class="page-title">{title}</h1>
      {#if meta}<div class="meta">{@render meta()}</div>{/if}
    </div>
    {#if description}<p class="page-description">{description}</p>{/if}
  </div>
  {#if actions}<div class="actions">{@render actions()}</div>{/if}
</header>

<style>
  .page-head {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    justify-content: space-between;
    gap: 1rem 1.5rem;
    margin-bottom: 1.5rem;
  }

  .heading {
    min-width: min(100%, 20rem);
    flex: 1 1 28rem;
  }

  .title-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem 0.875rem;
  }

  .page-title {
    font-size: clamp(1.6rem, 3.2vw, 2.125rem);
  }

  .page-description {
    margin-top: 0.5rem;
  }

  .meta,
  .actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
  }
</style>
