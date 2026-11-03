const root = ['plugins'] as const;

export const pluginKeys = {
  /** Every plugin view: installed plugins and unconfined executables. */
  root,
  list: () => [...root, 'list'] as const,
  unconfined: () => [...root, 'unconfined'] as const,
  mcpExecutables: () => [...root, 'mcp-executables'] as const,
  index: () => [...root, 'index'] as const
};
