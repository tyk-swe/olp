import { createContext } from 'svelte';

/// Back/forward cursor pagination state shared by the list pages. `history`
/// holds the cursor of every previous page (undefined = first page) so the
/// "previous" control can pop back without a server round-trip.
export type CursorHistory = {
  cursor: string | undefined;
  history: Array<string | undefined>;
};

export function emptyCursorHistory(): CursorHistory {
  return { cursor: undefined, history: [] };
}

export function pushCursor(
  state: CursorHistory,
  next: string | null | undefined
) {
  if (!next) return;
  state.history = [...state.history, state.cursor];
  state.cursor = next;
}

export function popCursor(state: CursorHistory) {
  state.cursor = state.history.at(-1);
  state.history = state.history.slice(0, -1);
}

export function resetCursor(state: CursorHistory) {
  state.cursor = undefined;
  state.history = [];
}

/**
 * Page state shared through Svelte context so a list keeps its cursor (and
 * filters) while the operator visits a detail route and comes back. The
 * console layout owns one instance per list for the whole session.
 */
export function listState<T extends object>(empty: () => T) {
  const [get, set] = createContext<T>();
  return { get, set, empty };
}

/** A list's cursor history plus the filter form and the query last applied. */
export type FilteredListState<Form, Query> = CursorHistory &
  Form & { applied: Query };

export type FilteredListSpec<Form extends object, Query> = {
  emptyForm: () => Form;
  toQuery: (form: Form) => Query;
  /** A message that blocks applying the form, if it is inconsistent. */
  validate?: (form: Form) => string | null;
};

export function filteredListState<Form extends object, Query>(
  spec: FilteredListSpec<Form, Query>
) {
  const [get, set] = createContext<FilteredListState<Form, Query>>();
  const empty = (): FilteredListState<Form, Query> => ({
    ...emptyCursorHistory(),
    ...spec.emptyForm(),
    applied: spec.toQuery(spec.emptyForm())
  });
  return {
    get,
    set,
    empty,
    /**
     * Restarts paging and applies the form. Returns the validation message
     * that blocked it instead, leaving the applied query untouched.
     */
    apply(state: FilteredListState<Form, Query>): string | null {
      const problem = spec.validate?.(state) ?? null;
      if (problem) return problem;
      resetCursor(state);
      state.applied = spec.toQuery(state);
      return null;
    },
    clear(state: FilteredListState<Form, Query>) {
      Object.assign(state, empty());
    }
  };
}

export function cursorPaginationProps(
  state: CursorHistory,
  next: string | null | undefined,
  onPageChange?: () => void
) {
  return {
    page: state.history.length + 1,
    hasPrevious: state.history.length > 0,
    hasNext: Boolean(next),
    onPrevious: () => {
      if (state.history.length === 0) return;
      popCursor(state);
      onPageChange?.();
    },
    onNext: () => {
      if (!next) return;
      pushCursor(state, next);
      onPageChange?.();
    }
  };
}
