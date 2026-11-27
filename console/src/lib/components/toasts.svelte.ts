export type ToastTone = 'success' | 'info' | 'warning' | 'danger';

export type Toast = {
  readonly id: number;
  readonly message: string;
  readonly tone: ToastTone;
};

export type ToastHold = 'pointer' | 'focus';

type Countdown =
  | {
      readonly state: 'running';
      readonly timer: ReturnType<typeof setTimeout>;
      readonly deadline: number;
    }
  | {
      readonly state: 'held';
      readonly remaining: number;
      readonly holds: ReadonlySet<ToastHold>;
    };

const DEFAULT_DURATION_MS = 5000;
const MIN_RESUME_MS = 600;

class ToastQueue {
  items = $state<readonly Toast[]>([]);
  #next = 0;
  #countdowns = new Map<number, Countdown>();

  push(message: string, tone: ToastTone = 'success') {
    this.#next += 1;
    const id = this.#next;
    this.items = [...this.items, { id, message, tone }];
    this.#countdowns.set(id, this.#start(id, DEFAULT_DURATION_MS));
  }

  hold(id: number, reason: ToastHold) {
    const countdown = this.#countdowns.get(id);
    if (!countdown) return;
    if (countdown.state === 'held') {
      this.#countdowns.set(id, {
        ...countdown,
        holds: new Set([...countdown.holds, reason])
      });
      return;
    }
    clearTimeout(countdown.timer);
    this.#countdowns.set(id, {
      state: 'held',
      remaining: Math.max(MIN_RESUME_MS, countdown.deadline - Date.now()),
      holds: new Set([reason])
    });
  }

  release(id: number, reason: ToastHold) {
    const countdown = this.#countdowns.get(id);
    if (countdown?.state !== 'held') return;
    const holds = new Set(
      [...countdown.holds].filter((hold) => hold !== reason)
    );
    this.#countdowns.set(
      id,
      holds.size
        ? { ...countdown, holds }
        : this.#start(id, countdown.remaining)
    );
  }

  dismiss(id: number) {
    this.#stop(id);
    this.#countdowns.delete(id);
    this.items = this.items.filter((toast) => toast.id !== id);
  }

  clear() {
    for (const id of this.#countdowns.keys()) this.#stop(id);
    this.#countdowns.clear();
    this.items = [];
  }

  #start(id: number, duration: number): Countdown {
    return {
      state: 'running',
      timer: setTimeout(() => this.dismiss(id), duration),
      deadline: Date.now() + duration
    };
  }

  #stop(id: number) {
    const countdown = this.#countdowns.get(id);
    if (countdown?.state === 'running') clearTimeout(countdown.timer);
  }
}

export const toasts = new ToastQueue();

export const notify = (message: string, tone: ToastTone = 'success') =>
  toasts.push(message, tone);
