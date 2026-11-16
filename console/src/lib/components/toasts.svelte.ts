type Toast = {
  readonly id: number;
  readonly message: string;
};

class ToastQueue {
  items = $state<readonly Toast[]>([]);
  #next = 0;

  push(message: string) {
    this.#next += 1;
    const id = this.#next;
    this.items = [...this.items, { id, message }];
    setTimeout(() => this.dismiss(id), 5000);
  }

  dismiss(id: number) {
    this.items = this.items.filter((toast) => toast.id !== id);
  }
}

export const toasts = new ToastQueue();

export const notify = (message: string) => toasts.push(message);
