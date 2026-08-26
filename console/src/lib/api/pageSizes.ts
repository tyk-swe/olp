/**
 * Rows requested per cursor page. A list and the state that drives it read the
 * same constant, so a page never disagrees with the request that filled it.
 */
export const AUDIT_PAGE_SIZE = 50;
export const GATEWAY_EPOCH_PAGE_SIZE = 25;
export const MEDIA_JOB_PAGE_SIZE = 25;
export const PROVIDER_CREDENTIAL_PAGE_SIZE = 100;
export const PROVIDER_HEALTH_PAGE_SIZE = 200;
export const PROVIDER_PAGE_SIZE = 50;
export const PROVIDER_REVISION_PAGE_SIZE = 25;
export const REQUEST_PAGE_SIZE = 25;
export const ROUTE_PAGE_SIZE = 50;
export const ROUTE_REVISION_PAGE_SIZE = 100;
export const RUNTIME_GENERATION_PAGE_SIZE = 25;
