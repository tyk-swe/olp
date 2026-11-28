import { resolve } from '$app/paths';
import type { NavigationSection } from './types.js';

export const AGENTS_SECTION: NavigationSection = {
  label: 'Agents',
  items: [
    {
      label: 'Agents and tools',
      href: resolve('agents'),
      icon: 'agent',
      description: 'Toolsets, agents and prompts you publish to callers',
      tabs: [
        {
          label: 'Toolsets',
          href: resolve('agents/toolsets'),
          route: 'GET /api/v1/toolsets'
        },
        {
          label: 'Agents',
          href: resolve('agents/definitions'),
          route: 'GET /api/v1/agents'
        },
        {
          label: 'Prompts',
          href: resolve('agents/prompts'),
          route: 'GET /api/v1/prompts'
        }
      ]
    },
    {
      label: 'MCP servers',
      href: resolve('mcp-servers'),
      icon: 'server',
      route: 'GET /api/v1/mcp-servers',
      description: 'Tool servers your agents and toolsets can call'
    },
    {
      label: 'Activity',
      href: resolve('activity'),
      icon: 'activity',
      description: 'What agents and tools did, and on whose behalf',
      tabs: [
        {
          label: 'Runs',
          href: resolve('activity/runs'),
          route: 'GET /api/v1/agent-runs'
        },
        {
          label: 'Tool calls',
          href: resolve('activity/tool-calls'),
          route: 'GET /api/v1/mcp-tool-calls'
        },
        {
          label: 'Agent-to-agent tasks',
          href: resolve('activity/tasks'),
          route: 'GET /api/v1/a2a-tasks'
        },
        {
          label: 'Delegations',
          href: resolve('activity/delegations'),
          route: 'GET /api/v1/agent-delegations'
        }
      ]
    },
    {
      label: 'Investigations',
      href: resolve('investigations'),
      icon: 'search',
      route: 'GET /api/v1/investigations',
      description: 'Ask an agent to explain what happened in a time window'
    },
    {
      label: 'My connections',
      href: resolve('agents/grants'),
      icon: 'link',
      route: 'GET /api/v1/mcp-user-grants',
      description: 'Tool accounts you have connected for agents to act as you'
    }
  ]
};
