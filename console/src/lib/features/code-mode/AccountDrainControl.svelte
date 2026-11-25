<script lang="ts">
  import { useQueryClient } from '@tanstack/svelte-query';
  import { confirmAction } from '#lib/components/confirm.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { setCodeAccountDrain, type CodeAccount } from './api';
  import { codeKeys } from './codeKeys';

  let { account, disabled }: { account: CodeAccount; disabled: boolean } =
    $props();
  const client = useQueryClient();
  const refresh = () => client.invalidateQueries({ queryKey: codeKeys.root });
  const action = useAction({ refreshOnConflict: refresh });

  async function toggleDrain() {
    const target = { id: account.id, etag: account.etag };
    const draining = !account.draining;
    if (
      !disabled &&
      (await confirmAction({
        title: draining ? `Drain ${account.name}?` : `Resume ${account.name}?`,
        body: draining
          ? 'New conversations and model pins will skip this account. Existing model pins can still use it.'
          : 'This account can take new conversations again when eligible.',
        confirmLabel: draining ? 'Start drain' : 'Resume routing'
      }))
    ) {
      await action.run(() => setCodeAccountDrain(target, draining), refresh);
    }
  }
</script>

<button
  class="button button-secondary"
  type="button"
  disabled={disabled || action.busy}
  onclick={toggleDrain}
>
  {account.draining ? 'Resume routing' : 'Start drain'}
</button>
{#if action.error}<p role="alert" class="inline-problem">{action.error}</p>{/if}
