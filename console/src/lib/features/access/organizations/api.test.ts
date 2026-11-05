import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '$lib/management/schema';
import { json, route, serve } from '$lib/management/test/transport';
import type { Project, ProjectMember } from '$lib/features/access/projects/api';
import {
  createOrganization,
  createOrganizationProject,
  listOrganizations,
  organizationMembers,
  organizationProjects,
  projectMembers,
  putOrganizationMember,
  putProjectMember,
  removeOrganizationMember,
  removeProjectMember,
  renameOrganization,
  renameProject,
  type Organization
} from './api';

type Schemas = components['schemas'];

const organization: Organization = {
  id: 'organization-id',
  name: 'Operations',
  etag: 'organization-version',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  created_by: 'owner-id',
  project_count: 1
};
const project: Project = {
  id: 'project-id',
  name: 'Project',
  etag: 'project-version',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  created_by: 'owner-id',
  created_by_email: 'owner@example.test',
  member_count: 1,
  organization_id: organization.id
};
const member: Schemas['OrganizationMember'] = {
  user_id: 'member-id',
  email: 'member@example.test',
  display_name: 'Member',
  role: 'viewer',
  installation_role: 'developer',
  active: true
};
const projectMember: ProjectMember = {
  user_id: member.user_id,
  email: member.email,
  display_name: member.display_name,
  role: 'developer',
  project_role: 'viewer',
  active: true,
  added_by: organization.created_by,
  added_by_email: 'owner@example.test',
  created_at: project.created_at
};
const updatedProjectMember: ProjectMember = {
  ...projectMember,
  project_role: 'manager'
};
const organizationPath = `/api/v1/organizations/${organization.id}`;
const projectPath = `${organizationPath}/projects/${project.id}`;
const lists = [
  {
    name: 'organizations',
    path: '/api/v1/organizations',
    first: organization,
    second: { ...organization, id: 'another-organization' },
    load: listOrganizations
  },
  {
    name: 'organization members',
    path: `${organizationPath}/members`,
    first: member,
    second: { ...member, user_id: 'another-member' },
    load: (signal?: AbortSignal) => organizationMembers(organization.id, signal)
  },
  {
    name: 'organization projects',
    path: `${organizationPath}/projects`,
    first: project,
    second: { ...project, id: 'another-project' },
    load: (signal?: AbortSignal) =>
      organizationProjects(organization.id, signal)
  },
  {
    name: 'project members',
    path: `${projectPath}/members`,
    first: projectMember,
    second: { ...projectMember, user_id: 'another-member' },
    load: (signal?: AbortSignal) =>
      projectMembers(organization.id, project.id, signal)
  }
];

afterEach(() => vi.unstubAllGlobals());

describe('organization API', () => {
  it.each(lists)(
    'loads all pages of $name',
    async ({ path, first, second, load }) => {
      const handler = vi.fn((request: Request) => {
        route(request, 'GET', path);
        const cursor = new URL(request.url).searchParams.get('cursor');
        return cursor
          ? json({ items: [second], next_cursor: null })
          : json({ items: [first], next_cursor: 'next-page' });
      });
      serve(handler, ['read']);

      await expect(load()).resolves.toEqual([first, second]);
      expect(handler).toHaveBeenCalledTimes(2);
      expect(
        new URL(handler.mock.calls[1][0].url).searchParams.get('cursor')
      ).toBe('next-page');
    }
  );

  it.each(lists)(
    'stops when $name returns a repeated cursor',
    async ({ path, load }) => {
      const handler = vi.fn((request: Request) => {
        route(request, 'GET', path);
        return json({
          items: [],
          next_cursor: handler.mock.calls.length <= 2 ? 'same-page' : null
        });
      });
      serve(handler, ['read']);

      await expect(load()).rejects.toMatchObject({
        problem: { type: 'urn:olp:problem:invalid-cursor-cycle' }
      });
      expect(handler).toHaveBeenCalledTimes(2);
    }
  );

  it('creates organizations and their projects with idempotency keys', async () => {
    const handler = vi.fn(async (request: Request) => {
      const path = new URL(request.url).pathname;
      route(request, 'POST', path);
      expect(request.headers.get('Idempotency-Key')).toBe('create-request');
      expect(await request.json()).toEqual({
        name: path.endsWith('/projects') ? project.name : organization.name
      });
      return json(path.endsWith('/projects') ? project : organization);
    });
    serve(handler, ['configure']);

    await expect(
      createOrganization(organization.name, 'create-request')
    ).resolves.toEqual(organization);
    await expect(
      createOrganizationProject(organization.id, project.name, 'create-request')
    ).resolves.toEqual(project);
    expect(
      handler.mock.calls.map(([request]) => new URL(request.url).pathname)
    ).toEqual(['/api/v1/organizations', `${organizationPath}/projects`]);
  });

  it('renames organizations and projects against the displayed revision', async () => {
    const handler = vi.fn(async (request: Request) => {
      const path = new URL(request.url).pathname;
      route(request, 'PATCH', path);
      expect(request.headers.get('If-Match')).toBe(
        path === organizationPath
          ? '"organization-version"'
          : '"project-version"'
      );
      expect(await request.json()).toEqual({ name: 'New name' });
      return json(path === organizationPath ? organization : project);
    });
    serve(handler, ['configure']);

    await expect(
      renameOrganization(organization.id, 'New name', organization.etag)
    ).resolves.toEqual(organization);
    await expect(
      renameProject(organization.id, project.id, 'New name', project.etag)
    ).resolves.toEqual(project);
    expect(
      handler.mock.calls.map(([request]) => new URL(request.url).pathname)
    ).toEqual([organizationPath, projectPath]);
  });

  it('adds and removes organization and project members with the owning revision', async () => {
    const handler = vi.fn(async (request: Request) => {
      const path = new URL(request.url).pathname;
      expect(path).toBe(
        path.startsWith(projectPath)
          ? `${projectPath}/members/${member.user_id}`
          : `${organizationPath}/members/${member.user_id}`
      );
      route(request, request.method === 'PUT' ? 'PUT' : 'DELETE', path);
      expect(request.headers.get('If-Match')).toBe(
        path.startsWith(projectPath)
          ? '"project-version"'
          : '"organization-version"'
      );
      if (request.method === 'PUT') {
        expect(await request.json()).toEqual({ role: 'manager' });
        return json(updatedProjectMember);
      }
      expect(request.method).toBe('DELETE');
      return new Response(null, { status: 204 });
    });
    serve(handler, ['configure']);

    await putOrganizationMember(
      organization.id,
      member.user_id,
      'manager',
      organization.etag
    );
    await expect(
      putProjectMember(
        organization.id,
        project.id,
        member.user_id,
        'manager',
        project.etag
      )
    ).resolves.toEqual(updatedProjectMember);
    await removeOrganizationMember(
      organization.id,
      member.user_id,
      organization.etag
    );
    await removeProjectMember(
      organization.id,
      project.id,
      member.user_id,
      project.etag
    );
    expect(handler).toHaveBeenCalledTimes(4);
  });
});
