<script lang="ts">
  import { operationKinds } from './history/api.js';
  import type { listProviders } from '#lib/features/providers/index.js';
  import type { listProjects } from '#lib/features/access/index.js';

  let {
    providerId = $bindable(),
    model = $bindable(),
    operation = $bindable(),
    inputTokens = $bindable(),
    outputTokens = $bindable(),
    cachedInputTokens = $bindable(),
    cacheWriteTokens = $bindable(),
    cacheWrite5mTokens = $bindable(),
    cacheWrite1hTokens = $bindable(),
    audioInputTokens = $bindable(),
    cachedAudioInputTokens = $bindable(),
    audioOutputTokens = $bindable(),
    imageInputTokens = $bindable(),
    cachedImageInputTokens = $bindable(),
    imageOutputTokens = $bindable(),
    mediaUnits = $bindable(),
    startedAt = $bindable(),
    projectId = $bindable(),
    batch = $bindable(),
    successful = $bindable(),
    toolCalls = $bindable(),
    providers,
    projects,
    busy,
    onSubmit
  }: {
    providerId: string;
    model: string;
    operation: string;
    inputTokens: string;
    outputTokens: string;
    cachedInputTokens: string;
    cacheWriteTokens: string;
    cacheWrite5mTokens: string;
    cacheWrite1hTokens: string;
    audioInputTokens: string;
    cachedAudioInputTokens: string;
    audioOutputTokens: string;
    imageInputTokens: string;
    cachedImageInputTokens: string;
    imageOutputTokens: string;
    mediaUnits: string;
    startedAt: string;
    projectId: string;
    batch: boolean;
    successful: boolean;
    toolCalls: { name: string; count: string }[];
    providers: Awaited<ReturnType<typeof listProviders>>;
    projects: Awaited<ReturnType<typeof listProjects>>;
    busy: boolean;
    onSubmit: () => void;
  } = $props();
</script>

<div class="calculator-form">
  <label for="calc-provider">Provider</label><select
    id="calc-provider"
    bind:value={providerId}
    >{#each providers as provider (provider.id)}<option value={provider.id}
        >{provider.name}</option
      >{/each}</select
  >
  <label for="calc-model">Model</label><input
    id="calc-model"
    bind:value={model}
  />
  <label for="calc-operation">Operation</label><select
    id="calc-operation"
    bind:value={operation}
    >{#each operationKinds as option (option)}<option value={option}
        >{option.replaceAll('_', ' ')}</option
      >{/each}</select
  >
  <label for="calc-input">Input tokens</label><input
    id="calc-input"
    bind:value={inputTokens}
    inputmode="numeric"
  />
  <label for="calc-output">Output tokens</label><input
    id="calc-output"
    bind:value={outputTokens}
    inputmode="numeric"
  />
  <label for="calc-cached">Cached input tokens</label><input
    id="calc-cached"
    bind:value={cachedInputTokens}
    inputmode="numeric"
  />
  <details class="more-usage">
    <summary>More usage</summary>
    <label for="calc-cache-write">Cache write tokens</label><input
      id="calc-cache-write"
      bind:value={cacheWriteTokens}
      inputmode="numeric"
    />
    <label for="calc-cache-write-5m">5-minute cache writes</label><input
      id="calc-cache-write-5m"
      bind:value={cacheWrite5mTokens}
      inputmode="numeric"
    />
    <label for="calc-cache-write-1h">1-hour cache writes</label><input
      id="calc-cache-write-1h"
      bind:value={cacheWrite1hTokens}
      inputmode="numeric"
    />
    <label for="calc-audio-input">Audio input tokens</label><input
      id="calc-audio-input"
      bind:value={audioInputTokens}
      inputmode="numeric"
    />
    <label for="calc-cached-audio-input">Cached audio input tokens</label><input
      id="calc-cached-audio-input"
      bind:value={cachedAudioInputTokens}
      inputmode="numeric"
    />
    <label for="calc-audio-output">Audio output tokens</label><input
      id="calc-audio-output"
      bind:value={audioOutputTokens}
      inputmode="numeric"
    />
    <label for="calc-image-input">Image input tokens</label><input
      id="calc-image-input"
      bind:value={imageInputTokens}
      inputmode="numeric"
    />
    <label for="calc-cached-image-input">Cached image input tokens</label><input
      id="calc-cached-image-input"
      bind:value={cachedImageInputTokens}
      inputmode="numeric"
    />
    <label for="calc-image-output">Image output tokens</label><input
      id="calc-image-output"
      bind:value={imageOutputTokens}
      inputmode="numeric"
    />
    {#each toolCalls as row, index (index)}
      <div class="tool-call-row">
        <label class="sr-only" for="calc-tool-name-{index}">Tool name</label
        ><input
          id="calc-tool-name-{index}"
          bind:value={row.name}
          placeholder="tool name"
        />
        <label class="sr-only" for="calc-tool-count-{index}">Calls</label><input
          id="calc-tool-count-{index}"
          bind:value={row.count}
          inputmode="numeric"
          placeholder="count"
        />
        <button
          type="button"
          class="link"
          aria-label="Remove tool call"
          onclick={() => toolCalls.splice(index, 1)}>Remove</button
        >
      </div>
    {/each}
    <button
      type="button"
      class="link"
      onclick={() => toolCalls.push({ name: '', count: '1' })}
      >Add tool call</button
    >
  </details>
  <label for="calc-media">Media units</label><input
    id="calc-media"
    bind:value={mediaUnits}
    inputmode="decimal"
  />
  <label for="calc-started">Started at</label><input
    id="calc-started"
    type="datetime-local"
    bind:value={startedAt}
  />
  <label for="calc-project">Consumer project</label><select
    id="calc-project"
    bind:value={projectId}
    ><option value="">Installation card</option
    >{#each projects as project (project.id)}<option value={project.id}
        >{project.name}</option
      >{/each}</select
  >
  <label class="check"
    ><input type="checkbox" bind:checked={batch} />Batch discount applies</label
  >
  <label class="check"
    ><input type="checkbox" bind:checked={successful} />Successful attempt</label
  >
  <button disabled={busy} onclick={onSubmit}
    >{busy ? 'Pricing…' : 'Quote'}</button
  >
</div>

<style>
  .calculator-form {
    display: grid;
    grid-template-columns: 12rem minmax(0, 20rem);
    gap: 0.6rem 1rem;
    align-items: center;
    margin: 1rem 0;
  }
  .check {
    grid-column: 2;
  }
  button {
    width: fit-content;
  }
  .more-usage {
    grid-column: 1 / -1;
    display: grid;
    grid-template-columns: 12rem minmax(0, 20rem);
    gap: 0.6rem 1rem;
    align-items: center;
  }
  .more-usage summary {
    grid-column: 1 / -1;
    cursor: pointer;
    color: var(--foreground-muted);
  }
  .tool-call-row {
    display: contents;
  }
  .tool-call-row > * {
    grid-column: auto;
  }
  .link {
    width: fit-content;
    padding: 0;
    background: none;
    border: none;
    color: var(--accent);
    cursor: pointer;
    text-decoration: underline;
  }
</style>
