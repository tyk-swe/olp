export type FieldErrors = Record<string, string[]>;

export type ProblemDetails = {
  type?: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  errors?: FieldErrors;
};

export class ApiProblem extends Error {
  readonly problem: ProblemDetails;

  constructor(problem: ProblemDetails) {
    super(problem.detail ?? problem.title);
    this.name = 'ApiProblem';
    this.problem = problem;
  }
}

export function throwApiProblem(error: unknown, response: Response): never {
  const problem = (error && typeof error === 'object' ? error : {}) as Partial<ProblemDetails>;
  throw new ApiProblem({
    type: problem.type ?? 'about:blank',
    title: problem.title ?? `Request failed (${response.status})`,
    status: problem.status ?? response.status,
    detail: problem.detail,
    errors: problem.errors
  });
}
