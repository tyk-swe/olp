<script lang="ts">
  import type { RouteDraftEditorState } from './routeDraftEditor.svelte';
  import {
    emptyPassthrough,
    parsePassthroughLines,
    passthroughMethods,
    validatePassthroughConfig,
    type PassthroughConfig
  } from './routePassthrough';
  let { editor }: { editor: RouteDraftEditorState } = $props();
  const config = $derived(editor.behavior.passthrough ?? null);
  const problem = $derived(validatePassthroughConfig(config));
  function update(next: PassthroughConfig | null) {
    editor.behavior = { ...editor.behavior, passthrough: next };
    editor.touch();
  }
  function toggle(enabled: boolean) {
    if (enabled) {
      update(emptyPassthrough());
      editor.operations = ['passthrough'];
      editor.maxAttempts = 1;
    } else {
      update(null);
    }
  }
  function patch(change: Partial<PassthroughConfig>) {
    if (config) update({ ...config, ...change });
  }
  function toggleMethod(method: string, checked: boolean) {
    if (!config) return;
    patch({
      methods: checked
        ? [...config.methods, method as PassthroughConfig['methods'][number]]
        : config.methods.filter((item) => item !== method)
    });
  }
</script>

<section
  class="card passthrough-editor"
  aria-labelledby="route-passthrough-heading"
>
  <p class="eyebrow">Escape hatch</p>
  <h2 id="route-passthrough-heading">Governed pass-through</h2>
  <label class="toggle">
    <input
      type="checkbox"
      checked={config !== null}
      disabled={!editor.canManage || Boolean(editor.busy)}
      onchange={(event) => toggle(event.currentTarget.checked)}
    />
    Serve unmodeled provider endpoints through this route
  </label>
  {#if config}
    <p class="inline-problem" role="note">
      Uncertified. This route advertises uncertified, binds exactly one
      connection, and cannot use fallback, mirroring, translation, response
      caching or guardrails.
    </p>
    <div class="form-field">
      <label for="passthrough-dialect">Native dialect segment</label>
      <input
        id="passthrough-dialect"
        value={config.dialect}
        disabled={!editor.canManage}
        oninput={(event) => patch({ dialect: event.currentTarget.value })}
      />
      <small
        >Callers use /native/&#123;dialect&#125;/routes/&#123;route&#125;/… and
        keys must name this route.</small
      >
    </div>
    <fieldset class="form-field">
      <legend>Methods</legend>
      {#each passthroughMethods as method (method)}
        <label>
          <input
            type="checkbox"
            checked={config.methods.includes(method)}
            disabled={!editor.canManage}
            onchange={(event) =>
              toggleMethod(method, event.currentTarget.checked)}
          />
          {method}
        </label>
      {/each}
    </fieldset>
    <div class="form-field">
      <label for="passthrough-paths">Path patterns, one per line</label>
      <textarea
        id="passthrough-paths"
        rows="4"
        value={config.paths.join('\n')}
        disabled={!editor.canManage}
        onchange={(event) =>
          patch({ paths: parsePassthroughLines(event.currentTarget.value) })}
      ></textarea>
      <small>Anchored segment patterns. * matches one nonempty segment.</small>
    </div>
    <div class="form-field">
      <label for="passthrough-query">Allowed query names, one per line</label>
      <textarea
        id="passthrough-query"
        rows="3"
        value={(config.query ?? []).join('\n')}
        disabled={!editor.canManage}
        onchange={(event) =>
          patch({ query: parsePassthroughLines(event.currentTarget.value) })}
      ></textarea>
    </div>
    <div class="form-field">
      <label for="passthrough-headers">Forwarded headers, one per line</label>
      <textarea
        id="passthrough-headers"
        rows="3"
        value={(config.headers ?? []).join('\n')}
        disabled={!editor.canManage}
        onchange={(event) =>
          patch({ headers: parsePassthroughLines(event.currentTarget.value) })}
      ></textarea>
      <small
        >Hop-by-hop and caller credential headers are always stripped. Only the
        bound credential is injected.</small
      >
    </div>
    <label class="toggle">
      <input
        type="checkbox"
        checked={config.acknowledge_opaque_unmetered}
        disabled={!editor.canManage}
        onchange={(event) =>
          patch({ acknowledge_opaque_unmetered: event.currentTarget.checked })}
      />
      I acknowledge this endpoint may create opaque provider state and unmetered work
    </label>
    {#if problem}<p class="inline-problem" role="alert">{problem}</p>{/if}
  {/if}
</section>

<style>
  .passthrough-editor {
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 1rem;
    font-size: 1rem;
    font-weight: 500;
  }
  .toggle {
    display: flex;
    gap: 0.5rem;
    align-items: flex-start;
  }
</style>
