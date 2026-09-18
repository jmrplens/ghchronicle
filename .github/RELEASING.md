# Releasing

A release is a tag. Everything else is a workflow, and the list below is what
is not.

## Before the tag

- [ ] `VERSION` and the tag agree. The release workflow's preflight job refuses
      the tag otherwise, and it is the first thing it checks.
- [ ] `go build ./... && go vet ./... && go test -race ./...` and
      `golangci-lint run ./...` are green on `main`.
- [ ] `go run ./cmd/gen_dashboards -check` writes nothing.
- [ ] Every panel has been run against a store holding a real account, not
      only against the fixture:

      ```sh
      GRAFANA_TOKEN=... make check-dashboards-live STORE=influxdb DS=<datasource-uid> RANGE=now-10y
      ```

      This is the one check the offline ones cannot stand in for. A column of
      InfluxDB, PostgreSQL or Elasticsearch exists once a point has carried it,
      and the fixture carries every field of every measurement while a real
      account only carries what has happened to it: a panel selecting a field
      nobody has ever had a reason to write is refused outright, and Grafana
      draws "No data" with a corner badge nobody notices. That is how "Time to
      resolve an alert" hid eight resolved alerts behind an empty panel for a
      release, on an account that had only ever fixed alerts and so had never
      written `dismissed_reason`.

      It exits 0 when no panel names a column the store has not created, so it
      is a gate and not a reading exercise. A panel over a family the store has
      not collected yet is reported as `WAIT` and counted on its own line,
      because that fills itself; `EMPTY` is not a failure either, a panel can
      be honestly empty. Anything under `FAIL` is the release's problem.

      Run it for each store you have a datasource for. It needs a Grafana, so
      CI can never do it.

      **It can only find a refused column on InfluxDB and PostgreSQL.** On
      Elasticsearch, Prometheus and Graphite a missing field is not an error:
      the query answers nothing, so the same defect arrives as `EMPTY` and the
      run passes. A green run against those three says every panel is
      answerable, not that every column exists. What covers them is the
      containerised suite, which runs every panel against stores where every
      family has been written and fails on any panel error not on an explicit
      allowlist (`test/e2e/docker`, behind the `dockere2e` tag); the release
      workflow calls `e2e.yml` as a job the release needs, so it has already
      run by the time a tag publishes anything.

      **The class is open, and this check only closes it for the account it is
      run against.** Some panels name a column the collector writes only when
      something has happened, and they draw here only because this account has
      done it. The whole list, with the condition each is written under, is
      `conditionalColumns` in
      `cmd/internal/dashboards/conditional_columns_test.go`, and a test keeps it
      complete: it parses every collector for a field written under a condition,
      intersects that with every column the two SQL dashboards name, and fails
      when the two agree on something the list does not carry. The ones an
      ordinary account meets, worth knowing by name:

      - `label_names` (`pulls.go`), in "Open the longest", "Open issues the
        longest" and "Largest merged pull requests": an account that has never
        labelled an issue or a pull request loses three tables, two of them the
        lists this dashboard's own round of fixes was about.
      - `seconds_to_merge` (`pulls.go`), in "Merged and closed in range" and
        three more: a fresh install, and anyone who pushes to main rather than
        merging, has never written it.
      - `seconds_to_first_human_review` (`pulls.go`), in "Merged and closed in
        range": fourteen rows in the whole of the store this was measured
        against, so an account that works alone loses that panel.
      - `never_used` and `days_to_expiry` in "Account keys"
        (`internal/collect/profile.go`), `days_since_use` in "Deploy keys"
        (`internal/collect/settings.go`), `environment_url` in "Deployments by
        environment" (`internal/collect/deployments.go`).

      None of them has an unconditional column that says the same thing, the way
      `alert_state` replaced `dismissed_reason`, so they stay. If this check
      reports a refused column on somebody else's store, look at that list
      first. What would find them before a user does is a second end-to-end
      fixture for an account that has done none of the optional things; the one
      we have carries every field of every measurement, which is exactly why it
      cannot.
- [ ] `cd site && pnpm run build && pnpm run lint` is green, which also holds
      the published counts to the code.
- [ ] The documentation says what this version does, not what the last one did.
      A new family, a new sink or a new setting is on its pages in both
      languages, and `docs/` has been regenerated from them.
- [ ] `config.example.yaml` names every setting. A test enforces this; it is
      listed here because it is the one people forget.

## The tag

```sh
git tag -a v2.0.0 -m "v2.0.0" && git push origin v2.0.0
```

That runs `.github/workflows/release.yml`: the end-to-end and race suites, then
GoReleaser, which builds the binaries for the three operating systems and two
architectures, signs the checksums and the SBOMs with cosign keylessly, pushes
the image to `ghcr.io`, pushes it to Docker Hub when the two Docker Hub secrets
are set, and writes the release notes from the commit subjects.

## After the tag

- [ ] The release page lists the archives, the checksums, the signatures and
      the SBOMs, and the notes read as notes.
- [ ] `docker run --rm ghcr.io/jmrplens/ghchronicle:v2.0.0 -version` prints the
      version. The workflow checks this too, and it is worth seeing once.
- [ ] Move the major tag, which is what `uses: jmrplens/ghchronicle@v2`
      resolves through:

      ```sh
      git tag -f -a v2 v2.0.0^{} -m "v2" && git push -f origin v2
      ```

      `^{}` because `v2.0.0` is an annotated tag, and a tag pointing at a tag
      is not what `@v2` should resolve through. `-a -m` because a repository
      configured to sign its tags makes every `git tag` annotated, and an
      annotated tag with no message is an error rather than a prompt.

      The release workflow listens for three-part tags, so this move starts
      nothing. With one exception, met once: GitHub reads the workflow file
      at the ref being pushed, so a major tag moved onto a commit that predates
      that filter runs the old file. The preflight job refuses it, before
      anything is published, which is what it is for.

- [ ] Publish the Action to the Marketplace from the release page, on a first
      release. [ACTION.md](ACTION.md) has the steps and the categories.
- [ ] Docker Hub: the description and the README of the repository there are
      not pushed by the workflow, so update them after the first tag and after
      any change to what the image expects.
- [ ] The Grafana directory: five listings, one per store, and a regeneration
      that changes panels is a new revision of the same five rather than five
      new ones. `dashboards/PUBLISHING.md` has the detail.
- [ ] The repository's `homepage` field points at
      <https://jmrp.io/docs/ghchronicle/>, and the topics are set. That URL
      301s to the Pages one: the canonical domain is what a fiche, a scrape
      or a citation carries, and the reader still lands on the docs.

## When something goes wrong

A tag that failed halfway leaves images published that nothing announces, which
is why the release concurrency group does not cancel in progress. Fix forward
with a new patch tag rather than moving the failed one: a moved tag is a
different binary under a name somebody may already have pinned.
