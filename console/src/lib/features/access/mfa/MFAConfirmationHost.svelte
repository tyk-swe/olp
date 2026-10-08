<script lang="ts">
  import { onDestroy } from 'svelte';
  import SecretDialog from '$lib/components/SecretDialog.svelte';
  import MFAFlow from './MFAFlow.svelte';
  import { mfaPrompt } from './prompt.svelte';
  function cancel() {
    mfaPrompt.current?.finish(
      undefined,
      new Error('Verification was cancelled.')
    );
  }
  onDestroy(cancel);
</script>

{#if mfaPrompt.current}
  {#key mfaPrompt.current.id}
    {@const prompt = mfaPrompt.current}
    <SecretDialog
      eyebrow="Account security"
      title="Multi-factor authentication"
      description="Complete verification to continue. Authentication challenges and recovery codes stay in this window."
      onClose={() =>
        prompt.finish(undefined, new Error('Verification was cancelled.'))}
    >
      <MFAFlow
        challenge={prompt.challenge}
        enrollment={prompt.enrollment}
        onComplete={(result) => prompt.finish(result)}
      />
    </SecretDialog>
  {/key}
{/if}
