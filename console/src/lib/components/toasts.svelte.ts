type Toast = {
  readonly id: number;
  readonly message: string;
};

class ToastQueue {
  items = $state<readonly Toast[]>([]);
  #next = 0;
  #timers = new Map<number, ReturnType<typeof setTimeout>>();

  push(message: string) {
    this.#next += 1;
    const id = this.#next;
    this.items = [...this.items, { id, message }];
    this.#timers.set(
      id,
      setTimeout(() => this.dismiss(id), 5000)
    );
  }

  dismiss(id: number) {
    const timer = this.#timers.get(id);
    if (timer !== undefined) {
      clearTimeout(timer);
      this.#timers.delete(id);
    }
    this.items = this.items.filter((toast) => toast.id !== id);
  }

  clear() {
    for (const timer of this.#timers.values()) clearTimeout(timer);
    this.#timers.clear();
    this.items = [];
  }
}

export const toasts = new ToastQueue();

export const notify = (message: string) => toasts.push(message);
