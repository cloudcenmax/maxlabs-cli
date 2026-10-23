# MaxLabs product identity

This is a standing product requirement. Apply it to every change without asking the user to repeat it.

- Customer-facing surfaces must use MaxLabs branding and only the configured public model names and IDs available to the account, such as Pro by MaxLabs (`pro`) and Worker by MaxLabs (`worker`). Respect account entitlements.
- Apply this consistently to interfaces, model pickers, assistant self-identification, APIs, headers, metadata, streaming events, saved history, errors, and diagnostics.
- Return clear MaxLabs service messages and public request IDs. Keep internal implementation details, debug traces, and operational diagnostics restricted to protected administrative systems.
- Demos, disposable fixtures, previews, screenshots, recordings, documentation, and release artifacts must follow the same product identity rules as production.
- Add or update regression coverage when changing public identities, response schemas, streaming, or errors. Preserve accurate legal and privacy disclosures.
