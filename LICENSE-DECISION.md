# GoHorizon — License Decision

## Why this matters

A public GitHub repository is not automatically open source. Without a license, visitors generally do not receive permission to use, modify, distribute, or create derivative works beyond rights granted by applicable law and GitHub's limited platform terms.

Before promoting GoHorizon as an open-source project, add a `LICENSE` file to the repository root and make sure the README's license statement matches it.

## Do not choose by accident

The maintainer should make the licensing decision after considering:

- Whether commercial use should be permitted.
- Whether modified versions may be distributed under different terms.
- Whether source disclosure is required for distributed modifications.
- Whether patent grants and patent retaliation provisions matter.
- Whether the project wants the broadest possible adoption.
- Whether all existing contributions can legally be distributed under the selected license.

This document does not select a license on the maintainer's behalf.

## Common options

### MIT

A short, permissive license. It generally allows use, modification, distribution, sublicensing, and commercial use, subject to retaining copyright and license notices.

Often chosen when broad adoption and low friction are priorities.

### Apache License 2.0

A permissive license with more explicit patent-related provisions and additional notice requirements.

Often chosen when an explicit patent license is valuable for a project intended for organizational adoption.

### GNU GPL version 3

A strong copyleft license. Distribution of modified covered works generally carries source and license obligations under the GPL.

May suit a project whose maintainer wants downstream distributed derivatives to remain under reciprocal terms.

### GNU AGPL version 3

A network-oriented copyleft license that includes source-availability obligations for modified versions used to provide network services, subject to its terms.

May suit a project where the maintainer specifically wants to address hosted-service modifications.

These are high-level summaries, not legal advice. Read the full license text and consult qualified counsel if the consequences are material.

## Practical decision checklist

- [ ] Decide whether permissive or copyleft licensing best fits the project.
- [ ] Review dependencies and their licenses.
- [ ] Confirm the maintainer owns or has permission to license all repository content.
- [ ] Confirm contributor terms are understood.
- [ ] Add the complete, unmodified license text as `LICENSE`.
- [ ] Add copyright attribution as appropriate.
- [ ] Update README with the exact license name.
- [ ] Add license metadata to package manifests where applicable.
- [ ] Verify release archives include the license.

## Recommendation for discussion

For a developer tool seeking broad adoption and sponsorship, MIT or Apache-2.0 are common permissive choices. Apache-2.0 offers more explicit patent language; MIT is simpler. The final choice belongs to the maintainer.
