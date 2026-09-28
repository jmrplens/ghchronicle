## What this changes

<!-- One paragraph. What it does, and why that is the right thing to do. -->

## How it was checked

<!--
Which of these ran, and what they said. A behaviour change wants a test that
fails before it and passes after it: name the test.
-->

- [ ] `make build vet test-race`
- [ ] `make golangci-lint` (the formatter's diff as well as the linters)
- [ ] `make check-dashboards check-gallery check-layouts check-config-options check-compose check-config-cases`
      (for a Go change; regenerate with the same targets without `check-`,
      the dashboards with `go run ./cmd/gen_dashboards` or
      `make gen-dashboards`, and `make config-options` before
      `make config-cases`)
- [ ] `cd site && pnpm run build && pnpm run lint` (for a change under `site/`)

## What it owes

<!-- Delete the lines that do not apply. -->

- [ ] A new setting is in `config.example.yaml` and on the configuration
      pages, and `make config-options` then `make config-cases` have run.
- [ ] A new measurement or family is in the measurements page, the cadence
      table and the cost table, in both languages, and the counts stated in
      prose still match the code (`pnpm run stats:check`, part of the lint).
- [ ] A new field on an existing measurement is a field, not a tag.
- [ ] A new page has its Spanish twin, with the same headings.
- [ ] Any number stated in prose was measured, and says what against.
- [ ] No em dash or en dash, and nothing in the repository is in Spanish
      outside `site/src/content/docs/es/`.
