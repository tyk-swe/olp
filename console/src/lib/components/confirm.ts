import { mount, unmount } from 'svelte';
import ConfirmDialog from '#lib/components/ConfirmDialog.svelte';

export type Confirmation = {
  readonly title: string;
  readonly body: string;
  readonly confirmLabel: string;
  readonly danger?: boolean;
};

export function confirmAction(confirmation: Confirmation): Promise<boolean> {
  return new Promise((settle) => {
    const instance = mount(ConfirmDialog, {
      target: document.body,
      props: {
        ...confirmation,
        onSettle(confirmed: boolean) {
          void unmount(instance);
          settle(confirmed);
        }
      }
    });
  });
}
