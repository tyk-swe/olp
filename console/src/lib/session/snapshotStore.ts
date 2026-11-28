import {
  anonymousAuthenticationSnapshot,
  reduceAuthentication,
  type AuthenticationAction,
  type AuthenticationSnapshot
} from '#lib/session/state.js';

type Listener = (snapshot: AuthenticationSnapshot) => void;

export class SnapshotStore {
  private value: AuthenticationSnapshot = anonymousAuthenticationSnapshot();
  private listeners = new Set<Listener>();

  current(): AuthenticationSnapshot {
    return this.value;
  }

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener);
    listener(this.value);
    return () => this.listeners.delete(listener);
  }

  apply(action: AuthenticationAction): void {
    this.replace(reduceAuthentication(this.value, action));
  }

  replace(snapshot: AuthenticationSnapshot): void {
    this.value = snapshot;
    for (const listener of this.listeners) listener(snapshot);
  }

  reset(): void {
    this.listeners.clear();
    this.value = anonymousAuthenticationSnapshot();
  }
}
