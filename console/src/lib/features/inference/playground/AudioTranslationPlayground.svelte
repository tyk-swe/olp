<script lang="ts">
  import { onDestroy } from 'svelte';
  import {
    translateAudio,
    translationFormats,
    type TranslationFormat
  } from './audioTranslation';

  let { route }: { route: string } = $props();
  let key = $state('');
  let files = $state<FileList>();
  let prompt = $state('');
  let temperature = $state('');
  let format = $state<TranslationFormat>('json');
  let busy = $state(false);
  let error = $state('');
  let result = $state<string | null>(null);
  let abort: AbortController | null = null;

  async function run(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !files?.[0]) return;
    busy = true;
    error = '';
    result = null;
    abort = new AbortController();
    try {
      result = await translateAudio(
        route,
        key,
        files[0],
        prompt,
        temperature,
        format,
        abort.signal
      );
    } catch (reason) {
      error =
        reason instanceof Error ? reason.message : 'Audio translation failed.';
    } finally {
      busy = false;
      abort = null;
    }
  }
  onDestroy(() => {
    abort?.abort();
    key = '';
  });
</script>

<form class="card audio-translation" onsubmit={run}>
  <p class="eyebrow">Public audio translation · billed on execution</p>
  <h2>Translate audio into English</h2>
  <p>
    The original file is uploaded to the selected route. Empty optional controls
    use the provider's defaults.
  </p>
  <div class="form-field">
    <label for="translation-key">Inference API key</label>
    <input
      id="translation-key"
      type="password"
      bind:value={key}
      autocomplete="off"
      spellcheck="false"
    />
    <small>Held only in this tab and sent to the same-origin public API.</small>
  </div>
  <div class="form-field">
    <label for="translation-file">Audio file</label>
    <input
      id="translation-file"
      type="file"
      accept="audio/*,.mp4,.webm"
      bind:files
    />
  </div>
  <div class="form-field">
    <label for="translation-prompt">English prompt (optional)</label>
    <textarea id="translation-prompt" bind:value={prompt} rows="3"></textarea>
  </div>
  <div class="form-field">
    <label for="translation-temperature">Temperature (optional)</label>
    <input
      id="translation-temperature"
      type="text"
      inputmode="decimal"
      bind:value={temperature}
    />
  </div>
  <div class="form-field">
    <label for="translation-format">Response format</label>
    <select id="translation-format" bind:value={format}>
      {#each translationFormats as item (item)}<option value={item}
          >{item}</option
        >{/each}
    </select>
  </div>
  <div class="actions">
    <button
      class="button button-primary"
      type="submit"
      disabled={busy || !key || !route || !files?.[0]}
      >{busy ? 'Translating…' : 'Run billable translation'}</button
    >
    {#if busy}<button
        class="button button-secondary"
        type="button"
        onclick={() => abort?.abort()}>Cancel delivery</button
      >{/if}
    {#if result !== null}<button
        class="button button-secondary"
        type="button"
        onclick={() => {
          result = null;
        }}>Clear result</button
      >{/if}
  </div>
  {#if error}<p role="alert" class="inline-problem">{error}</p>{/if}
  {#if result !== null}<pre
      aria-label="Native audio translation result">{result}</pre>{/if}
</form>

<style>
  .audio-translation {
    display: grid;
    gap: 1rem;
    padding: 1.5rem;
    margin-top: 1rem;
    min-width: 0;
  }
  h2,
  p {
    margin: 0;
  }
  pre {
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
</style>
