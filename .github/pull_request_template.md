## What and why

<!-- What changes and why. Link the issue or the PROMPT.md phase. -->

## How it was tested

<!-- Tests added or changed; manual checks (make up && make smoke, curl ...). -->

## Checklist

- [ ] `make lint test` passes locally (CI also runs integration tests, scans and the e2e smoke test)
- [ ] Tests come with the code
- [ ] New loops, queues and external calls have their metrics (`docs/metrics.md`)
- [ ] Migrations are backward compatible: the previous version runs on the new schema
- [ ] No secrets; new settings are in `.env.example` and the README
- [ ] Non-obvious choices are recorded in `docs/decisions.md`
- [ ] Contract with `uptime-infra` (ports, probes, metrics, image, env) unchanged, or `uptime-infra` is updated too
