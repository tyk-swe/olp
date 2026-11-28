import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import {
  listAllProjectMembers,
  listProjectMemberships,
  listProjects,
  type Project,
  type ProjectMember
} from './api.js';

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

describe('project reads', () => {
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
});
