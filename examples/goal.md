# Improve a command-line data importer

Inspect the repository before proposing changes. Determine which requirements
are already implemented and evidenced; do not rebuild existing behavior.

Required capabilities:
- Correct quoted CSV fields and useful malformed-input errors.
- A dry-run mode that validates rows without changing stored data.
- Idempotent import so replaying the same input does not duplicate records.

Propose small dependency-ordered features with observable acceptance scenarios.
Separate unknown product semantics into explicit questions. No changes to
production, credentials, release policy, or external spending are authorized.
