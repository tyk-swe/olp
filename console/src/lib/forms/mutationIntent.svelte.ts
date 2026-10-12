import { onDestroy } from 'svelte';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { MutationIntent, MutationIntents } from './mutationIntent';

/** A submission belongs to its mounted form and authenticated principal. */
export function useMutationIntent() {
  const intent = new MutationIntent();
  usePrincipalLifetime(() => intent.reset());
  return intent;
}

export function useMutationIntents() {
  const intents = new MutationIntents();
  usePrincipalLifetime(() => intents.reset());
  return intents;
}

/** Capture a mounted form's authority lifetime before asynchronous work. */
export function usePrincipalLifetime(reset: () => void) {
  let epoch = 0;
  let active = true;
  const retire = () => {
    epoch += 1;
    reset();
  };
  const unsubscribe = authLifecycle.onPrincipalChange(retire);
  onDestroy(() => {
    active = false;
    unsubscribe();
    retire();
  });
  return () => {
    const currentEpoch = epoch;
    return () => active && currentEpoch === epoch;
  };
}
