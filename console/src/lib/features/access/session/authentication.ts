import { confirmMFA } from '$lib/features/access/mfa/prompt.svelte';
import {
  acceptInvitation as submitInvitation,
  login as submitPassword,
  currentSession,
  type AuthenticationStep,
  type CurrentSession
} from './api';

async function completeAuthentication(
  step: AuthenticationStep,
  signal?: AbortSignal
): Promise<CurrentSession> {
  if (step.kind === 'authenticated') return step.session;
  await confirmMFA({ kind: 'challenge', challenge: step.challenge }, signal);
  return currentSession(signal);
}

export async function login(
  ...input: Parameters<typeof submitPassword>
): Promise<CurrentSession> {
  return completeAuthentication(await submitPassword(...input), input[2]);
}

export async function acceptInvitation(
  ...input: Parameters<typeof submitInvitation>
): Promise<CurrentSession> {
  return completeAuthentication(await submitInvitation(...input), input[1]);
}
