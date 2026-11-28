# Contributing to GoHorizon

Thank you for considering a contribution. GoHorizon is intended to become a dependable queue operations and worker orchestration platform, and high-quality contributions to code, tests, documentation, and issue triage are welcome.

## Before you start

1. Read the README and current roadmap.
2. Search existing issues and pull requests before opening a duplicate.
3. For substantial changes, open an issue or discussion first so the design can be agreed before significant work begins.
4. Check the repository's license. Do not contribute code unless you are comfortable contributing under the project's selected license. If no license has yet been added, ask the maintainer before submitting code intended for distribution.

## Development setup

The exact development commands and prerequisites must be taken from the current repository documentation and build files. Do not assume commands in an old handoff are still accurate.

Before submitting a change:

- Run the relevant Go tests.
- Run the relevant Laravel/PHP tests when changing the package or integration.
- Run dashboard checks and tests when changing React code.
- Run formatting, linting, and static checks defined by the repository.
- Add or update tests for behavior changes.
- Update documentation and configuration examples where needed.

In your pull request, list the commands you ran and identify any checks you could not run.

## Architectural expectations

GoHorizon separates the control plane from brokers and application runtimes.

- Keep broker-specific behavior inside broker adapters.
- Keep framework/runtime-specific behavior inside runtime integrations.
- Expose capabilities explicitly.
- Preserve the native semantics of each broker.
- Avoid claiming exactly-once processing or other guarantees that the underlying integration cannot provide.
- Keep the initial Laravel/Redis path working while extending the architecture.
- Avoid introducing distributed control-plane complexity while the supported deployment model is single deployment.

## Pull request guidelines

A useful pull request should:

- Solve one coherent problem.
- Explain the motivation and user-visible impact.
- Describe important design choices.
- Include tests for new behavior and regressions.
- Update relevant documentation.
- Mention configuration changes and migration implications.
- Call out security or compatibility implications.
- Avoid unrelated formatting or refactoring.

For UI changes, include screenshots where practical. For API changes, document request/response behavior and compatibility implications.

## Reporting bugs

Include:

- GoHorizon version or commit.
- Operating system and deployment method.
- Relevant Go, PHP, Laravel, Redis, and package versions.
- Steps to reproduce.
- Expected and actual behavior.
- Relevant logs with secrets and personal data removed.
- Whether the issue is reproducible with a minimal configuration.

Never publish passwords, tokens, private keys, session cookies, or sensitive production data.

## Proposing features

Explain:

1. The operational problem.
2. Who experiences it and in what environment.
3. Which broker/runtime integrations are affected.
4. Whether the feature depends on broker-specific semantics.
5. Security and authorization implications.
6. How the feature could be tested.

A feature request is a proposal, not a commitment to implement it.

## Code of conduct

Be respectful, constructive, and welcoming. Critique ideas and code rather than people. Harassment, discrimination, intimidation, and abusive behavior are not acceptable.

If a separate `CODE_OF_CONDUCT.md` is adopted, follow its reporting and enforcement process.
