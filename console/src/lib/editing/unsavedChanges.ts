import { beforeNavigate } from '$app/navigation';
import { replaceEqualDeep } from '@tanstack/svelte-query';

let decision: { navigation: unknown; discard: boolean } | null = null;

export function hasChanges<Draft>(saved: Draft, edited: Draft): boolean {
  return replaceEqualDeep(saved, edited) !== saved;
}

/** Local transitions must ask before replacing the same draft that route
 * navigation protects. */
export function confirmDiscardChanges(isDirty: boolean): boolean {
  return !isDirty || confirm('Discard unsaved changes?');
}

export function guardUnsavedChanges(isDirty: () => boolean) {
  beforeNavigate((navigation) => {
    if (navigation.shallow && navigation.type === 'goto') return;
    if (
      navigation.type !== 'leave' &&
      navigation.from?.url.pathname === navigation.to?.url.pathname
    )
      return;
    if (!isDirty()) return;
    if (decision?.navigation !== navigation) {
      decision = { navigation, discard: confirmDiscardChanges(true) };
    }
    if (!decision.discard) navigation.cancel();
  });
}
