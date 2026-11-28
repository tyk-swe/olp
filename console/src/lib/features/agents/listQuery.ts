export function loadingWhileFetching(query: {
  isFetching: boolean;
  isError: boolean;
  error: unknown;
  refetch: () => Promise<unknown>;
}) {
  return {
    isPending: query.isFetching && !query.isError,
    isError: query.isError,
    error: query.error,
    refetch: () => query.refetch()
  };
}
