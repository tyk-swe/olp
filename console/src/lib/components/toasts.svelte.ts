export type ToastTone = 'success' | 'info' | 'warning' | 'danger';

export type Toast = {
  readonly id: number;
  readonly message: string;
  readonly tone: ToastTone;
};

type TimerState = {
  timer?: ReturnType<typeof setTimeout>;
  remaining: number;
  startedAt: number;
  paused: boolean;
};

const DEFAULT_DURATION_MS = 5000;
const MIN_RESUME_MS = 600;

class ToastQueue {
  items = $state<readonly Toast[]>([]);
  #next = 0;
  #timers = new Map<number, TimerState>();

  push(message: string, tone: ToastTone = 'success') {
    this.#next += 1;
    const id = this.#next;
    this.items = [...this.items, { id, message, tone }];
    this.#timers.set(id, {
      timer: setTimeout(() => this.dismiss(id), DEFAULT_DURATION_MS),
      remaining: DEFAULT_DURATION_MS,
      startedAt: Date.now(),
      paused: false
    });
  }

  pause(id: number) {
    const entry = this.#timers.get(id);
    if (!entry || entry.paused) return;
    if (entry.timer !== undefined) clearTimeout(entry.timer);
    entry.timer = undefined;
    entry.remaining = Math.max(
      MIN_RESUME_MS,
      entry.remaining - (Date.now() - entry.startedAt)
    );
    entry.paused = true;
  }

  resume(id: number) {
    const entry = this.#timers.get(id);
    if (!entry || !entry.paused) return;
    entry.paused = false;
    entry.startedAt = Date.now();
    entry.timer = setTimeout(() => this.dismiss(id), entry.remaining);
  }

  dismiss(id: number) {
    const entry = this.#timers.get(id);
    if (entry?.timer !== undefined) clearTimeout(entry.timer);
    this.#timers.delete(id);
    this.items = this.items.filter((toast) => toast.id !== id);
  }

  clear() {
    for (const entry of this.#timers.values()) {
      if (entry.timer !== undefined) clearTimeout(entry.timer);
    }
    this.#timers.clear();
    this.items = [];
  }
}

export const toasts = new ToastQueue();

export const notify = (message: string, tone: ToastTone = 'success') =>
  toasts.push(message, tone);
