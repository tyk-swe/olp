import { confirmMFA } from '#lib/features/access/mfa/prompt.svelte.js';
import { reauthenticateWithPassword as submitPassword } from './api';

export async function reauthenticateWithPassword(
  ...input: Parameters<typeof submitPassword>
): Promise<void> {
  const step = await submitPassword(...input);
  if (step.kind === 'mfa')
    await confirmMFA({ kind: 'challenge', challenge: step.challenge });
}
