export const pluginKeys = {
  /** Every plugin view: installed plugins and unconfined executables. */
  root: ['plugins'] as const,
  list: () => ['plugins', 'list'] as const,
  unconfined: () => ['plugins', 'unconfined'] as const,
  mcpExecutables: () => ['plugins', 'mcp-executables'] as const,
  index: () => ['plugins', 'index'] as const
};
