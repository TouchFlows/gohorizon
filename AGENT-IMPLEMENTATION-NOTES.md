# Coding-Agent Handoff — GoHorizon Sponsorship and Repository Presentation Refresh

## Objective

Apply the accompanying documentation and repository-metadata refresh to the existing GoHorizon repository. Improve clarity, discoverability, contributor onboarding, and sponsorship conversion without overstating implementation status or changing product behavior.

## Important constraints

1. **Audit before editing.** Inspect the current repository and reconcile every statement in the supplied drafts against the actual code, tests, CI, package manifests, and documentation.
2. Do not claim RabbitMQ or additional runtimes are supported unless their adapters are implemented, tested, and documented.
3. Do not claim exactly-once processing or uniform broker semantics.
4. Preserve existing working Laravel/Redis functionality.
5. Do not choose or add a license without explicit maintainer approval.
6. Do not modify GitHub Sponsors or Buy Me a Coffee tiers. The repository already has a `FUNDING.yml` pointing to `marktamis` on both services.
7. Do not create a release or tag without explicit maintainer approval.
8. Do not invent screenshots, performance numbers, compatibility claims, or security guarantees.

## Files to prepare/apply

- `README.md` — use the supplied draft as the structural and messaging baseline, but retain accurate installation, configuration, API, and development information from the existing README.
- `docs/ROADMAP.md` — add the public roadmap, clearly separating current state from planned work.
- `CONTRIBUTING.md` — add contributor workflow and testing expectations.
- `SECURITY.md` — add private vulnerability-reporting guidance consistent with repository settings.
- `REPOSITORY-METADATA.md` — reference for manual GitHub About description and topics; do not commit this file unless the maintainer wants it in the repository.
- `LICENSE-DECISION.md` — maintainer decision aid; do not treat it as the project's license.
- This handoff — do not commit unless desired.

## Repository audit

Before editing, inspect:

- Existing README, docs, handoff files, and project structure.
- Go modules, PHP Composer package, frontend package manifests.
- CI workflows and test commands.
- Actual implemented API routes and dashboard views.
- Worker lifecycle and broker operations.
- Authentication/authorization and audit behavior.
- Existing license files, dependency notices, and funding configuration.
- Existing contribution/security policies.

Use the current repository state as source of truth. If the supplied draft conflicts with code or tests, correct the draft rather than changing working product code to match marketing copy.

## README editing instructions

- Keep the opening concise and compelling.
- State clearly: “Starting with Laravel 13 + Redis.”
- Present GoHorizon as an operational control plane, not a broker replacement.
- Add a concise “What it does today” list based only on verified functionality.
- Add “Planned integrations” for RabbitMQ and other runtimes.
- Keep accurate quick-start instructions from the existing README.
- Move long implementation/API details into linked docs only if those docs exist or are created in this task.
- Add sponsorship links near the top or after the initial value proposition, and retain a shorter CTA near the bottom.
- Explain concrete sponsorship impact and clarify that sponsorship does not buy an SLA or guaranteed features.
- Keep existing badges only if they are valid and useful.
- Add screenshot/demo section only when real assets exist.

## Metadata instructions

Recommended GitHub About description:

> An open-source queue operations and worker orchestration platform. Built in Go, starting with Laravel and Redis, with a React dashboard and a roadmap towards multi-broker, multi-language support.

Suggested topics are in `REPOSITORY-METADATA.md`. Review each topic against GitHub's topic rules and project truth before applying. `rabbitmq` is a roadmap/discoverability topic, not a support claim.

## License gate

The public repository review found no root `LICENSE` file at that time. Recheck. If still absent:

- Do not select or generate a license automatically.
- Report the licensing decision as a maintainer blocker.
- Keep README wording neutral until a license is approved and added.

## Quality checks

- Verify all README links resolve.
- Verify setup commands against the current repository.
- Run relevant Go, PHP, and frontend checks where available.
- Check Markdown formatting and heading hierarchy.
- Ensure no future feature is described as implemented.
- Ensure funding links and `FUNDING.yml` remain correct.
- Review diff for accidental removal of important technical documentation.

## Final report

Return:

1. Files created or modified.
2. Claims removed or qualified because implementation was incomplete.
3. Tests/checks run and results.
4. Manual GitHub settings still requiring the maintainer (description/topics/social preview).
5. License decision required, if applicable.
6. Any screenshots/demo assets still needed.
7. Any discrepancies between the supplied drafts and current repository state.

Do not push or publish changes unless explicitly authorized.
