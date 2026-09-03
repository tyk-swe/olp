# Execution backlog

Seven evidence gaps carried from the
[September 2026 retrospective](archive/2026-09/README.md). Every item has a new
week and priority; none changes the shipped product surface.

## Week 1

- [ ] **TEST-01** Provision the protected live-provider environment, AWS and
  GCP workload identities, bounded credentials and quotas; record one green
  run and close the drift issue through a successful recovery — P1 · M · in progress
- [ ] **REL-07** Reproduce the 2.2.1-chart to 2.3.0 fresh-cluster Helm upgrade
  and record migration 0049 — P1 · S · planned

## Week 2

- [ ] **DOC-05** Record an unseen user's elapsed time from README checkout to
  first successful request — P2 · S · planned
- [ ] **OTEL-05** Record an end-to-end trace in the Jaeger Compose overlay — P2 · S · planned
- [ ] **SPEND-06** Demonstrate priced budget exhaustion through the Compose stack — P2 · S · planned
- [ ] **SPEND-07** Run and record the Toxiproxy HA budget-outage scenario — P2 · S · planned

## Week 3

- [ ] **OTEL-06** Compare the tracing-disabled benchmark against the
  pre-tracing baseline on the same host — P2 · S · planned

## Definition of done

- Link hosted run, issue, screenshot, or benchmark evidence from the item.
- Keep tests and security gates intact; evidence is not a reason for an ignore.
- Move unfinished work to a newly dated period rather than silently carrying
  its old schedule.
