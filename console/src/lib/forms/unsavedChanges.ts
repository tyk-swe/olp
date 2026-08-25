import { beforeNavigate } from '$app/navigation';

let decision: { navigation: unknown; discard: boolean } | null = null;

export function guardUnsavedChanges(isDirty: () => boolean) {
  beforeNavigate((navigation) => {
    if (!isDirty()) return;
    if (decision?.navigation !== navigation) {
      decision = { navigation, discard: confirm('Discard unsaved changes?') };
    }
    if (!decision.discard) navigation.cancel();
  });
}
