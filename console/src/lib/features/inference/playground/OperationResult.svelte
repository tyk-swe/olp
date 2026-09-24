<script lang="ts">
  import { stringifyNativeJSON } from '$lib/json/nativeJson';
  import {
    nativeCountFields,
    rerankRows,
    vectorRows
  } from './operationPresentation';

  let {
    operation,
    response,
    responseRaw,
    request
  }: {
    operation: string;
    response: unknown;
    responseRaw?: string;
    request?: Record<string, unknown>;
  } = $props();

  const vectors = $derived(
    operation === 'embeddings' ? vectorRows(response, request) : []
  );
  const ranks = $derived(
    operation === 'rerank' ? rerankRows(response, request) : []
  );
  const counts = $derived(
    operation === 'token_count' ? nativeCountFields(response) : []
  );
  const raw = $derived(responseRaw ?? stringifyNativeJSON(response));
  const media = $derived(
    operation.startsWith('image_') ||
      operation.startsWith('video_') ||
      operation === 'speech' ||
      operation === 'transcription' ||
      operation === 'translation'
  );
  const realtime = $derived(operation === 'realtime');

  function download() {
    const url = URL.createObjectURL(
      new Blob([raw], { type: 'application/json' })
    );
    try {
      const anchor = document.createElement('a');
      anchor.href = url;
      anchor.download = `${operation}-native-result.json`;
      anchor.click();
    } finally {
      URL.revokeObjectURL(url);
    }
  }
</script>

<div class="operation-result">
  <h3>Operation result · {operation.replaceAll('_', ' ')}</h3>
  {#if operation === 'embeddings'}
    <p class="scope-note">
      Native vector storage is retained. A declared dtype and logical dimension
      are shown separately from observed storage; unknown dimensions stay
      unknown. No vector is converted to float for display.
    </p>
    <div class="table-shell" role="region" aria-label="Vector shapes">
      <table class="data-table">
        <thead
          ><tr
            ><th>Result</th><th>Input index</th><th>Layout / dtype</th><th
              >Logical shape</th
            ><th>Storage shape</th></tr
          ></thead
        >
        <tbody>
          {#each vectors as vector (vector.position)}
            <tr>
              <td>{vector.position + 1}</td>
              <td><code>{vector.inputIndex}</code></td>
              <td>{vector.layout} · {vector.dtype}</td>
              <td>{vector.logicalShape}</td>
              <td>{vector.storageShape}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {:else if operation === 'rerank'}
    <p class="scope-note">
      Native rank order, input identity, scores, and ties are shown unchanged.
      Scores are not normalized or compared across models.
    </p>
    <div class="table-shell" role="region" aria-label="Native ranked results">
      <table class="data-table">
        <thead
          ><tr
            ><th>Rank</th><th>Input index</th><th>Input ID</th><th
              >Native score</th
            ></tr
          ></thead
        >
        <tbody>
          {#each ranks as rank (rank.position)}
            <tr>
              <td>{rank.position + 1}</td>
              <td><code>{rank.inputIndex}</code></td>
              <td><code>{rank.inputId}</code></td>
              <td><code>{rank.score}</code></td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {:else if operation === 'token_count'}
    <p class="scope-note">
      These are native counting-operation fields, separate from estimates and
      billed usage.
    </p>
    <dl>
      {#each counts as count (count.name)}<div>
          <dt>{count.name}</dt>
          <dd><code>{count.value}</code></dd>
        </div>{/each}
    </dl>
  {:else if media}
    <p class="scope-note">
      Media resources and lifecycle fields remain in the native result below.
      The response is not resized, transcoded, sampled, or decoded for preview.
    </p>
  {:else if realtime}
    <p class="scope-note">
      Native event order, turn boundaries, interruption, VAD, and media timing
      remain in the result below. No event is synthesized from a final message.
    </p>
  {/if}
  <details>
    <summary>Native result JSON</summary>
    <p class="scope-note">
      {responseRaw !== undefined
        ? 'Original provider JSON bytes, including numeric spelling and member order.'
        : 'Decoded JSON value; numeric spelling is retained when supplied by the current server.'}
    </p>
    <pre data-testid="native-operation-result">{raw}</pre>
    <button type="button" class="button button-secondary" onclick={download}>
      Download native result JSON
    </button>
  </details>
</div>

<style>
  .operation-result {
    display: grid;
    gap: 0.7rem;
    min-width: 0;
  }
  h3,
  .scope-note {
    margin: 0;
  }
  h3 {
    text-transform: capitalize;
  }
  .scope-note {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  dl {
    margin: 0;
  }
  dl div {
    display: flex;
    gap: 1rem;
    padding: 0.3rem 0;
  }
  dt {
    color: var(--foreground-muted);
  }
  dd {
    margin: 0;
  }
  details {
    margin-top: 0.5rem;
    min-width: 0;
  }
  .table-shell {
    min-width: 0;
    max-width: 100%;
  }
  .data-table {
    table-layout: fixed;
  }
  .data-table :is(th, td) {
    padding: 0.65rem 0.55rem;
    font-size: var(--text-caption);
  }
  summary {
    cursor: pointer;
  }
  pre {
    overflow: auto;
    max-height: 32rem;
    white-space: pre;
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
</style>
