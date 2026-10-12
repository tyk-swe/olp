import { beforeNavigate } from '$app/navigation';

let decision: { navigation: unknown; discard: boolean } | null = null;

/** Local transitions must ask before replacing the same draft that route
 * navigation protects. */
export function confirmDiscardChanges(isDirty: boolean): boolean {
  return !isDirty || confirm('Discard unsaved changes?');
}

export function guardUnsavedChanges(isDirty: () => boolean) {
  beforeNavigate((navigation) => {
    if (!isDirty()) return;
    if (decision?.navigation !== navigation) {
      decision = { navigation, discard: confirmDiscardChanges(true) };
    }
    if (!decision.discard) navigation.cancel();
  });
}
