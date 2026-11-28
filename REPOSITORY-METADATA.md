# GoHorizon — GitHub Repository Metadata

Suggested repository metadata based on the public repository review in November 2026.

## Repository description

Recommended description:

> An open-source queue operations and worker orchestration platform. Built in Go, starting with Laravel and Redis, with a React dashboard and a roadmap towards multi-broker, multi-language support.

This communicates the broader ambition while making the current starting point clear. It does not imply that RabbitMQ or other runtimes are already supported.

## Suggested topics

GitHub allows up to 20 topics. Suggested set:

```text
golang
laravel
laravel-horizon
redis
rabbitmq
message-queue
job-queue
queue-management
queue-monitoring
worker-management
worker-supervision
distributed-systems
background-jobs
observability
prometheus
react
php
```

Use `rabbitmq` as a discoverability topic only if the README and roadmap clearly label it as planned. Remove it if the project prefers topics to represent implemented integrations only.

Avoid irrelevant or overly broad tags. Reassess topics as integrations become real.

## Website

If a project documentation site or demo becomes available, add it as the repository website. Do not use a temporary or unrelated URL.

## Social preview

Create a project-specific social preview image showing:

- GoHorizon name and logo/mark.
- A concise tagline such as “Queue operations. Worker orchestration.”
- Go, Laravel, Redis, and React visual cues.
- No claim that planned integrations are already available.

## Releases and tags

The November 2026 review found no GitHub Releases published at that time. This does not establish whether Git tags exist.

Publish the first GitHub Release only when:

- Installation is reproducible.
- Required configuration is documented.
- Supported versions are stated.
- Tests pass in CI.
- Upgrade and known limitations are described.
- The release artifact or source tag is immutable and clearly versioned.

## Funding

The repository's `FUNDING.yml` was already configured with:

```yaml
github: marktamis
buy_me_a_coffee: marktamis
```

No change is required unless the maintainer's funding accounts change.

Suggested sponsorship links for README:

- https://github.com/sponsors/marktamis
- https://buymeacoffee.com/marktamis

## About description versus README

The About description should be short and searchable. The README should explain the product, current functionality, limitations, roadmap, installation, contribution process, and why sponsorship matters. Avoid trying to fit the whole project pitch into the repository description.
