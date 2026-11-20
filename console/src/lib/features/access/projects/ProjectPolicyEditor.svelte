<script lang="ts" generics="Doc extends { etag: string }, Form, Input">
  import { useAction } from '#lib/editing/action.svelte.js';
  import { untrack, type Component, type Snippet } from 'svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { projectKeys } from './projectKeys';

  let {
    name,
    savedMessage,
    help,
    queryKey,
    fetch,
    save,
    toForm,
    toInput,
    validate,
    fields: Fields,
    prefix,
    editable,
    extra
  }: {
    name: string;
    savedMessage: string;
    help: string;
    queryKey: readonly unknown[];
    fetch: (signal: AbortSignal) => Promise<Doc>;
    save: (etag: string, input: Input) => Promise<Doc>;
    toForm: (doc?: Doc) => Form;
    toInput: (form: Form) => Input;
    validate: (form: Form) => string;
    fields: Component<
      { form: Form; prefix: string; disabled?: boolean },
      object,
      'form'
    >;
    prefix: string;
    editable: boolean;
    extra?: Snippet<[Doc]>;
  } = $props();
  const client = useQueryClient();
  const policy = createQuery(() => ({
    queryKey,
    queryFn: ({ signal }) => fetch(signal)
  }));
  let form = $state(untrack(() => toForm()));
  let etag = $state('');
  let loaded = $state('');
  const action = useAction();

  function load(value: Doc) {
    form = toForm(value);
    etag = value.etag;
    loaded = JSON.stringify(toInput(form));
  }
  $effect(() => {
    if (
      policy.data &&
      (!etag ||
        (policy.data.etag !== etag && JSON.stringify(toInput(form)) === loaded))
    )
      load(policy.data);
  });

  async function reload() {
    const result = await policy.refetch();
    if (result.data) {
      load(result.data);
      action.error = '';
    }
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (action.busy || !editable) return;
    action.error = validate(form);
    if (action.error) return;
    await action.run(
      () => save(etag, toInput(form)),
      async (saved) => {
        client.setQueryData(queryKey, saved);
        load(saved);
        await client.invalidateQueries({ queryKey: projectKeys.root });
        notify(savedMessage);
      }
    );
  }
</script>

<section aria-label={`Project ${name}`}>
  {#if policy.isPending}
    <p role="status">Loading {name}…</p>
  {:else if policy.isError}
    <p class="inline-problem" role="alert">{errorMessage(policy.error)}</p>
    <button class="button button-secondary" type="button" onclick={reload}
      >Retry {name}</button
    >
  {:else if etag}
    <form onsubmit={submit}>
      <p class="muted">{help}</p>
      <Fields bind:form {prefix} disabled={action.busy || !editable} />
      {#if policy.data && extra}{@render extra(policy.data)}{/if}
      {#if action.error}<p class="inline-problem" role="alert">
          {action.error}
        </p>{/if}

      <div class="actions">
        {#if editable}<button
            class="button button-primary"
            type="submit"
            disabled={action.busy}
            >{action.busy ? 'Saving…' : `Save ${name}`}</button
          >{/if}
        <button
          type="button"
          class="button button-secondary"
          disabled={action.busy}
          onclick={reload}>Reload saved policy</button
        >
      </div>
    </form>
  {/if}
</section>

<style>
  section {
    margin: 1.5rem 0;
    padding: 1rem 0;
    border-top: 1px solid var(--border-hairline);
    border-bottom: 1px solid var(--border-hairline);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
</style>
