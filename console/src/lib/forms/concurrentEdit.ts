export type ConcurrentEditState = Readonly<{
  snapshotEtag: string | null;
  remoteEtag: string | null;
  dirty: boolean;
  conflict: boolean;
  reloadPending: boolean;
}>;

export type ReconcileResult = Readonly<{
  state: ConcurrentEditState;
  hydrate: boolean;
}>;

export type ConflictNoticeKind = 'conflict' | 'newer' | null;

export function initialConcurrentEdit(): ConcurrentEditState {
  return {
    snapshotEtag: null,
    remoteEtag: null,
    dirty: false,
    conflict: false,
    reloadPending: false
  };
}

export function reconcile(state: ConcurrentEditState, incomingEtag: string): ReconcileResult {
  if (state.remoteEtag === incomingEtag && !state.reloadPending) {
    return { state, hydrate: false };
  }
  if (state.reloadPending || state.snapshotEtag === null || !state.dirty) {
    return {
      state: {
        snapshotEtag: incomingEtag,
        remoteEtag: incomingEtag,
        dirty: false,
        conflict: false,
        reloadPending: false
      },
      hydrate: true
    };
  }
  return {
    state: {
      ...state,
      remoteEtag: incomingEtag,
      reloadPending: false
    },
    hydrate: false
  };
}

export function markDirty(state: ConcurrentEditState): ConcurrentEditState {
  return state.dirty ? state : { ...state, dirty: true };
}

export function markSaved(state: ConcurrentEditState, etag: string): ConcurrentEditState {
  return {
    snapshotEtag: etag,
    remoteEtag: etag,
    dirty: false,
    conflict: false,
    reloadPending: false
  };
}

export function markConflict(state: ConcurrentEditState): ConcurrentEditState {
  return state.conflict ? state : { ...state, conflict: true };
}

export function beginReload(state: ConcurrentEditState): ConcurrentEditState {
  return state.reloadPending ? state : { ...state, reloadPending: true };
}

export function acceptRemote(state: ConcurrentEditState, etag: string): ConcurrentEditState {
  if (state.conflict || state.remoteEtag !== state.snapshotEtag) {
    return {
      ...state,
      remoteEtag: etag,
      reloadPending: false
    };
  }
  return {
    ...state,
    snapshotEtag: etag,
    remoteEtag: etag,
    conflict: false,
    reloadPending: false
  };
}

export function conflictNotice(state: ConcurrentEditState): ConflictNoticeKind {
  if (state.conflict) return 'conflict';
  return state.dirty && state.remoteEtag !== state.snapshotEtag ? 'newer' : null;
}
