---
id: csv-quoted-fields
project: myapp
title: Preserve commas inside quoted CSV fields
kind: feature
risk: normal
priority: 20
depends_on: []
completion_target: reviewed
checks: [unit, smoke]
acceptance:
  - id: quoted-comma
    scenario: A quoted CSV field contains a comma.
    expected: The CLI emits a single field containing that comma.
  - id: malformed-quote
    scenario: A quoted field is not closed.
    expected: The CLI exits nonzero with a useful error and no success output.
---

Fix the existing CSV parsing behavior without changing the output schema.
Reuse the repository's current parser/library where possible. Exercise the
actual CLI, not just the parser in isolation. Do not change unrelated files.
