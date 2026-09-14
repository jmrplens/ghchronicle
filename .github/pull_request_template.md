## What this changes

<!-- One paragraph. What it does, and why that is the right thing to do. -->

## How it was checked

<!--
Which of these ran, and what they said. A behaviour change wants a test that
fails before it and passes after it: name the test.
-->

- [ ] `go build ./... && go vet ./... && go test -race ./...`
- [ ] `golangci-lint run ./...`
- [ ] `cd site && pnpm run build && pnpm run lint` (for a change under `site/`)
- [ ] `go run ./cmd/gen_dashboards -check` (for a change to the dashboards)

## What it owes

<!-- Delete the lines that do not apply. -->

- [ ] A new setting is in `config.example.yaml` and on the configuration pages.
- [ ] A new measurement or family is in the measurements page, the cadence
      table and the cost table, in both languages.
- [ ] A new page has its Spanish twin, with the same headings.
- [ ] Any number stated in prose was measured, and says what against.
- [ ] No em dash or en dash, and nothing in the repository is in Spanish
      outside `site/src/content/docs/es/`.
