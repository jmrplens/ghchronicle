# Releasing

A release is a tag. Everything else is a workflow, and the list below is what
is not.

## Before the tag

- [ ] `VERSION` and the tag agree. The release workflow's preflight job refuses
      the tag otherwise, and it is the first thing it checks.
- [ ] `go build ./... && go vet ./... && go test -race ./...` and
      `golangci-lint run ./...` are green on `main`.
- [ ] `go run ./cmd/gen_dashboards -check` writes nothing.
- [ ] `cd site && pnpm run build && pnpm run lint` is green, which also holds
      the published counts to the code.
- [ ] The documentation says what this version does, not what the last one did.
      A new family, a new sink or a new setting is on its pages in both
      languages, and `docs/` has been regenerated from them.
- [ ] `config.example.yaml` names every setting. A test enforces this; it is
      listed here because it is the one people forget.

## The tag

```sh
git tag -a v1.0.0 -m "v1.0.0" && git push origin v1.0.0
```

That runs `.github/workflows/release.yml`: the end-to-end and race suites, then
GoReleaser, which builds the binaries for the three operating systems and two
architectures, signs the checksums and the SBOMs with cosign keylessly, pushes
the image to `ghcr.io`, pushes it to Docker Hub when the two Docker Hub secrets
are set, and writes the release notes from the commit subjects.

## After the tag

- [ ] The release page lists the archives, the checksums, the signatures and
      the SBOMs, and the notes read as notes.
- [ ] `docker run --rm ghcr.io/jmrplens/ghchronicle:v1.0.0 -version` prints the
      version. The workflow checks this too, and it is worth seeing once.
- [ ] Move the major tag, which is what `uses: jmrplens/ghchronicle@v1`
      resolves through:

      ```sh
      git tag -f v1 v1.0.0 && git push -f origin v1
      ```

- [ ] Publish the Action to the Marketplace from the release page, on a first
      release. [ACTION.md](ACTION.md) has the steps and the categories.
- [ ] Docker Hub: the description and the README of the repository there are
      not pushed by the workflow, so update them after the first tag and after
      any change to what the image expects.
- [ ] The Grafana directory: five listings, one per store, and a regeneration
      that changes panels is a new revision of the same five rather than five
      new ones. `dashboards/PUBLISHING.md` has the detail.
- [ ] The repository's `homepage` field points at
      <https://jmrplens.github.io/ghchronicle/>, and the topics are set.

## When something goes wrong

A tag that failed halfway leaves images published that nothing announces, which
is why the release concurrency group does not cancel in progress. Fix forward
with a new patch tag rather than moving the failed one: a moved tag is a
different binary under a name somebody may already have pinned.
