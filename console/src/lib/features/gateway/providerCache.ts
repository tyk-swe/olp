type QueryInvalidator = {
  invalidateQueries(filters: { queryKey: readonly unknown[] }): Promise<unknown>;
};

export function invalidateProviderSummaries(queryClient: QueryInvalidator) {
  return Promise.all([
    queryClient.invalidateQueries({ queryKey: ['provider-page'] }),
    queryClient.invalidateQueries({ queryKey: ['providers'] })
  ]);
}

export function invalidateProviderModelConsumers(queryClient: QueryInvalidator) {
  return Promise.all([
    queryClient.invalidateQueries({ queryKey: ['provider-model-inventory-page'] }),
    queryClient.invalidateQueries({ queryKey: ['enabled-provider-models'] })
  ]);
}
