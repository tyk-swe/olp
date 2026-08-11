import { createContext } from 'svelte';
import type { CursorHistory } from '$lib/api/pagination';

export const [getProviderPagination, setProviderPagination] =
  createContext<CursorHistory>();
