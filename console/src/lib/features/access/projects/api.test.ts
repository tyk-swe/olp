import { afterEach, describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/management/test/transport.js';
import {
  createProject,
  getProjectAttributionBudgets,
  getProjectAttributionPolicy,
  getProjectEndUserPolicy,
  getProjectLimitTemplates,
  getProjectRouteGroups,
  listAllProjectMembers,
  listProjectMemberships,
  listProjects,
  putProjectAttributionBudgets,
  putProjectAttributionPolicy,
  putProjectEndUserPolicy,
  putProjectLimitTemplates,
  putProjectMember,
  putProjectRouteGroups,
  removeProjectMember,
  renameProject,
  type Project,
  type ProjectMember
} from './api';

const project: Project = {
  id: 'project-id',
  name: 'Project',
  etag: '018f1234-5678-7abc-8def-0123456789ab',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  created_by: 'owner-id',
  created_by_email: 'owner@example.test',
  member_count: 1
};
const member: ProjectMember = {
  user_id: 'member-id',
  email: 'member@example.test',
  display_name: 'Member',
  role: 'developer',
  project_role: 'manager',
  active: true,
  added_by: project.created_by,
  added_by_email: project.created_by_email,
  created_at: project.created_at
};
const projectPath = `/api/v1/projects/${project.id}`;
const policies = [
  {
    name: 'end-user policy',
    path: 'end-user-policy',
    body: { policy: { blocked: ['end-user-digest'] } },
    response: {
      policy: { blocked: ['end-user-digest'] },
      etag: 'policy-version'
    },
    read: () => getProjectEndUserPolicy(project.id),
    write: () =>
      putProjectEndUserPolicy(project.id, 'policy-version', {
        blocked: ['end-user-digest']
      })
  },
  {
    name: 'attribution policy',
    path: 'attribution-policy',
    body: { policy: { required_attribution_keys: ['team'] } },
    response: {
      policy: { required_attribution_keys: ['team'] },
      etag: 'policy-version'
    },
    read: () => getProjectAttributionPolicy(project.id),
    write: () =>
      putProjectAttributionPolicy(project.id, 'policy-version', {
        required_attribution_keys: ['team']
      })
  },
  {
    name: 'route groups',
    path: 'route-groups',
    body: { groups: { approved: ['route-a'] } },
    response: { groups: { approved: ['route-a'] }, etag: 'policy-version' },
    read: () => getProjectRouteGroups(project.id),
    write: () =>
      putProjectRouteGroups(project.id, 'policy-version', {
        approved: ['route-a']
      })
  },
  {
    name: 'limit templates',
    path: 'limit-templates',
    body: { templates: { default: {} } },
    response: { templates: { default: {} }, etag: 'policy-version' },
    read: () => getProjectLimitTemplates(project.id),
    write: () =>
      putProjectLimitTemplates(project.id, 'policy-version', { default: {} })
  },
  {
    name: 'attribution budgets',
    path: 'attribution-budgets',
    body: { budgets: { team: { research: { daily_cost_limit: '1.25' } } } },
    response: {
      budgets: { team: { research: { daily_cost_limit: '1.25' } } },
      usage: {},
      etag: 'policy-version'
    },
    read: () => getProjectAttributionBudgets(project.id),
    write: () =>
      putProjectAttributionBudgets(project.id, 'policy-version', {
        team: { research: { daily_cost_limit: '1.25' } }
      })
  }
];

afterEach(() => vi.unstubAllGlobals());

describe('project API', () => {
  it.each([
    {
      path: '/api/v1/projects',
      first: project,
      second: { ...project, id: 'other-project' },
      load: () => listProjects()
    },
    {
      path: `${projectPath}/members`,
      first: member,
      second: { ...member, user_id: 'other-member' },
      load: () => listAllProjectMembers(project.id)
    }
  ])('loads every page of $path', async ({ path, first, second, load }) => {
    const handler = vi.fn((request: Request) => {
      route(request, 'GET', path);
      const query = new URL(request.url).searchParams;
      expect(query.get('limit')).toBe('50');
      return query.has('cursor')
        ? json({ items: [second] })
        : json({ items: [first], next_cursor: 'next-page' });
    });
    serve(handler, ['read']);

    await expect(load()).resolves.toEqual([first, second]);
    expect(handler).toHaveBeenCalledTimes(2);
    expect(
      new URL(handler.mock.calls[1][0].url).searchParams.get('cursor')
    ).toBe('next-page');
  });

  it('loads the current principal’s project memberships', async () => {
    const membership = { id: project.id, name: project.name, role: 'manager' };
    serve(
      (request) => {
        route(request, 'GET', '/api/v1/project-memberships');
        return json({ items: [membership] });
      },
      ['read']
    );

    await expect(listProjectMemberships()).resolves.toEqual([membership]);
  });

  it('creates projects with the submitted name and idempotency key', async () => {
    serve(
      async (request) => {
        route(request, 'POST', '/api/v1/projects');
        expect(request.headers.get('Idempotency-Key')).toBe('create-request');
        expect(await request.json()).toEqual({ name: project.name });
        return json(project, 201);
      },
      ['configure']
    );

    await expect(
      createProject(project.name, 'create-request')
    ).resolves.toEqual(project);
  });

  it('quotes the displayed revision when renaming a project or changing its members', async () => {
    const handler = vi.fn(async (request: Request) => {
      const path = new URL(request.url).pathname;
      expect(path).toBe(
        request.method === 'PATCH'
          ? projectPath
          : `${projectPath}/members/${member.user_id}`
      );
      route(
        request,
        request.method === 'PATCH'
          ? 'PATCH'
          : request.method === 'PUT'
            ? 'PUT'
            : 'DELETE',
        path
      );
      expect(request.headers.get('If-Match')).toBe(`"${project.etag}"`);
      if (request.method === 'PATCH') {
        expect(await request.json()).toEqual({ name: 'Renamed' });
        return json({ ...project, name: 'Renamed' });
      }
      if (request.method === 'PUT') {
        expect(await request.json()).toEqual({ role: 'manager' });
        return json(member);
      }
      return new Response(null, { status: 204 });
    });
    serve(handler, ['configure']);

    await expect(renameProject(project, 'Renamed')).resolves.toMatchObject({
      name: 'Renamed'
    });
    await expect(
      putProjectMember(project, member.user_id, 'manager')
    ).resolves.toEqual(member);
    await expect(
      removeProjectMember(project, member.user_id)
    ).resolves.toBeUndefined();
    expect(handler).toHaveBeenCalledTimes(3);
  });

  it.each(policies)(
    'reads and updates $name with its own revision',
    async ({ path, body, response, read, write }) => {
      const handler = vi.fn(async (request: Request) => {
        route(
          request,
          request.method === 'GET' ? 'GET' : 'PUT',
          `${projectPath}/${path}`
        );
        if (request.method === 'PUT') {
          expect(request.headers.get('If-Match')).toBe('"policy-version"');
          expect(await request.json()).toEqual(body);
        }
        return json(response);
      });
      serve(handler, ['read', 'configure']);

      await expect(read()).resolves.toEqual(response);
      await expect(write()).resolves.toEqual(response);
      expect(handler).toHaveBeenCalledTimes(2);
    }
  );
});
