## Summary

<!-- What changes and why. -->

## Validation

- [ ] `make check` passes (see CONTRIBUTING.md for toolchain setup)
- [ ] `make db-test` passes, or this change touches no storage/query code
- [ ] Regenerated any affected artifacts and committed them:
      `.sqlx/` (`make sqlx-prepare`), `openapi/management.json` +
      `console/src/lib/api/schema.d.ts` (`make openapi`),
      `docs/assets/screenshots/` (`make screenshots`), and all four Playwright
      baselines under `console/tests/e2e/operations.spec.ts-snapshots/`
- [ ] Migrations (if any) are forward-only and sequential
- [ ] Helm values, schema, and templates changed together (`make helm-verify`),
      or this change touches no `deploy/helm/` files
