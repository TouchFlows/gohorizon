# Security Policy

Security is important to GoHorizon because it supervises background workers and may expose operational controls over application infrastructure.

## Supported versions

Until the project publishes its first stable release and a formal support matrix, security fixes are handled on a best-effort basis for the actively maintained development branch.

After stable releases begin, this section should be updated with explicit supported-version ranges and a security-fix policy.

## Reporting a vulnerability

Please do not report exploitable vulnerabilities in public issues, discussions, or pull requests.

Use GitHub's **Report a vulnerability** feature on the repository's Security tab, if enabled. If private vulnerability reporting is unavailable, contact the maintainer through a private channel listed on the maintainer's GitHub profile.

Include, where possible:

- A concise description of the vulnerability.
- Affected versions or commit.
- Preconditions and configuration required.
- Reproduction steps or a proof of concept.
- Potential impact.
- Any suggested mitigation.

Do not include real credentials, production data, or other people's personal information.

## What to expect

The maintainer will make a reasonable effort to acknowledge a private report, assess its severity, coordinate a fix, and publish an advisory when appropriate. Response and remediation times are not guaranteed while the project is independently maintained.

Please allow reasonable time for investigation and coordinated disclosure before making vulnerability details public.

## Security-sensitive areas

Examples include:

- Authentication, session management, and authorization.
- Worker process control and command construction.
- Redis connections, namespaces, and coordination locks.
- Telemetry ingestion and identity validation.
- Dashboard/API access and CSRF protections.
- Secrets, configuration files, logs, and audit records.
- Broker operations that can acknowledge, retry, delete, or otherwise alter jobs.

## Scope and limitations

This policy is not a guarantee that every deployment is secure or that every reported issue will qualify for a security advisory. Operators remain responsible for network exposure, secret management, access controls, updates, and secure configuration of their brokers and applications.
