# Changelog

What changed in each release, why, and what was left unproven.

Each section says what changed, the reason it changed, the measurement behind
it, and the part nobody verified. Where a claim here was measured, it says on
what. From 2.5.1 on, a release's section is its notes on GitHub word for word,
so every link in one is absolute; the release page adds the pull and
verification commands and a link to the commits.

Versions follow [semantic versioning](https://semver.org/). The dates are the
day the tag was pushed.

## 2.5.2 - 2026-09-26

Four things the store said that GitHub did not, each found by reading one
beside the other on the account this collects: a Loki line that called every
contribution merged, a count of threads commented elsewhere that was mostly
at home, five searches that stopped at a hundred, and archived repositories
whose stars froze at the last backfill.

- **A Loki line says what happened to the contribution.** The rendering of
  `gh_external_contribution` read the user, the repository and the number and
  nothing else, so every line said `USER merged OWNER/REPO#N`: open issues,
  open pull requests and pull requests closed without merging included. An
  item still open is stamped at the start of each day it is seen open, so
  each of them added a false "merged" line a day, and production held 36 in
  26 hours, 23 of them for issues. The sentence now follows the `kind` and
  `state` tags and the `merged` field: `USER's pull request OWNER/REPO#N` is
  open, was merged or was closed without merging, and
  `USER's issue OWNER/REPO#N` is open or was closed. Merging and closing are
  said of the item rather than put in the account's name, because the row
  does not say who did either, and in someone else's repository it is
  usually a maintainer. An open item says it is open, not that it was
  opened, since its line comes back every day it stays open: still one a
  day, because the sink keeps no state. A combination where the state and
  the field disagree, which the five searches do not produce, reads
  `USER's contribution OWNER/REPO#N` and leaves the rest to the logfmt tail.
  Lines already in Loki keep what they said.
- **`commented_elsewhere` leaves out the account's own repositories, and
  drops on upgrade.** It was searched as `commenter:LOGIN -author:LOGIN`,
  which leaves out the threads the account opened but not the repositories
  it owns, so every issue it answered and every Dependabot pull request it
  commented on at home counted as work in other people's. Its two siblings,
  `pulls_merged_elsewhere` and `issues_elsewhere`, already meant outside the
  account's repositories, with `-user:LOGIN`, and the Lifetime panel
  described all three that way. The query is now
  `commenter:LOGIN -user:LOGIN`, the same rule. The field keeps its name, so
  in every store the series drops by the difference on the first `totals`
  sweep after the upgrade: from 125 to 55 on the account it was measured on,
  where 103 of the 125 were in its own repositories and 54 of those were
  Dependabot's. Search counts an issue or a pull request once however many
  comments the account left on it, so the Lifetime tile now reads "Threads
  commented elsewhere", and the panel's description says what the two
  outside numbers count.
- **The outbound searches read past their first hundred.** The five searches
  behind `gh_external_contribution` asked for one page of a hundred and never
  read a cursor, on a sweep or on a backfill. An account past a hundred items
  in a state kept the newest hundred by creation date, an old pull request
  merged later never got its merged row because it was no longer among them,
  and `gh_account_total.pulls_merged_elsewhere`, which is GitHub's own count,
  disagreed with the table beside it. Nothing failed, so nothing said so. The
  two open states are now read to the end on every sweep, newest created
  first, since each open item gets a row for every day it stays open. The
  three closed states are ordered by what moved last, `sort:updated-desc`,
  because a merge or a close moves an item to the top however old it is: a
  sweep reads each back to a cadence before the previous outbound sweep, and
  a backfill until the pages run out or reach `backfill.since`. A page count
  would not have done for a sweep, since a hundred later updates, a bot
  locking old threads or a relabel, carry the item that closed onto a page
  nobody reads. GitHub serves a thousand results of any search and no more,
  so an account past a thousand in one state keeps the thousand that moved
  most recently, and the log now says so at warning, once per count, with
  the kind, the state, the count and what was read. A search that fails part
  way keeps the pages that answered. It costs a GraphQL point a page, so a
  sweep still spends eight until an account has more than a hundred open of
  a kind or more than a hundred move between two sweeps. Open items come
  back on the first sweep after the upgrade; an item closed before it that
  2.5.1 never read takes a
  [backfill](https://jmrp.io/docs/ghchronicle/how/backfill/), since a sweep
  reads back only to the one before it.
- **Archived repositories' stars and forks reach the account's totals on
  every sweep.** With `include_archived` off, the default, a sweep sets
  archived repositories aside and wrote only their `gh_repo_archived` row.
  Their `gh_repo` and `gh_repo_total` came from a backfill, once, stamped at
  the backfill, so their stars and forks froze there and left every panel
  once that instant left the range, and an install that never ran a backfill
  never had them. It rested on the premise that nothing about an archived
  repository moves, and people still star, unstar and fork them: on
  2026-09-26 jmrplens/FFT2octave's only row, from the backfill of 2026-09-18,
  said 4 stars where GitHub said 3, and the seventeen archived repositories
  the default filter sets aside on that account held 80 stars and 24 forks
  the Overview left out. The query every `totals` sweep already sent for
  their archive dates now also asks for the fields `gh_repo_total` is made
  of, and each gets that row beside its `gh_repo_archived`: the tags and
  fields a collected repository's row has, `archived` true, stamped at the
  sweep. Their history is still not walked, and a sweep writes them no
  `gh_repo` and no `gh_repo_policy`. The counts cost the gateway time, so the
  query carries twenty five repositories rather than fifty: a GraphQL point
  per twenty five archived repositories per `totals` sweep.
- **The Overview and "Every repository, ever" count them, in all five
  stores.** The Overview's stars and forks read a live repository's newest
  `gh_repo`, as before, and an archived one's newest `gh_repo_total`, one row
  per full name, so two owners' repositories of the same name are two, and
  one archived while the collector runs is counted once rather than under
  both its live row and its archived one. Under All the sums include the
  archived repositories the picker does not list, so on upgrade the tiles
  rise by what those hold, 80 stars and 24 forks on the account above; with
  repositories picked, those alone are counted, and a range shorter than the
  `totals` cadence, twelve hours by default, can leave the archived rows out.
  "Every repository, ever" lists them under All with their current counts,
  and a repository archived inside the range is one row flagged archived in
  every store, where Elasticsearch, Graphite and Prometheus drew it twice.
  The picker still lists live repositories only, because it reads `gh_repo`;
  since the SQL stores' All expands to that list, their filter lets the
  archived rows through when the variable's text is "All". In
  Elasticsearch the two sums also gained the repository filter they lacked.

Measured on 2026-09-26. The 36 Loki lines are production's stream over the
26 hours before the fix, and the five searches on this account answered 33
merged pull requests, all with `mergedAt`, and 69 items in the other four
states, none with it. `commented_elsewhere` read 125 through REST and through
the GraphQL alias the collector sends, and 55 with `-user:`; of the 102
threads the account had opened elsewhere, 33 count, because the opening post
is not a comment. On the searches: GitHub's default order is newest created
first, the same hundred in the same order as `sort:created-desc`, and
`sort:updated-desc` is honoured; 53 of the first hundred of one account's
closed issues by `updated-desc` had been opened before the oldest of the
first hundred by creation and closed after it, one opened in 2014 and closed
on 1 September 2026; 2,860 merged pull requests answered ten pages of a
hundred, with no item twice, and then no next page; `closedAt` was not after
`updatedAt` on any of 1,000 merged pull requests, and was on one of 373
closed unmerged, by a day, in 2011. The index's copy of `updatedAt` can lag
the item's own by years, facebook/flow pull requests updated in 2019 sorting
among 2017, which only makes the bound read a page more. Against the fifty
most starred archived repositories of google and of microsoft, at cost 1
every time, the lifetime row of fifty at once answered once in 9.2 seconds
and was refused twice with the gateway's 502 after 10.7 and 11.1, twenty five
answered in 5.6 to 6.7, this account's eighteen archived repositories, its
one archived fork among them, in 3.6 to 4.1, and the same fifty asked for the
scalars and the watchers alone in about a second.

Every fix carries a test shown to fail against the code before it. The
containerised suite gained a case against InfluxDB 3: a sweep sets an
archived repository aside, and the Overview's own SQL counts its stars under
All and not with the live repositories picked, and "Every repository, ever"
lists it, archived, with its stars.

Not verified, and worth saying plainly:

- None of it has run in production. The Loki sentences, the step down in
  `commented_elsewhere` and the archived rows are what the tests and the
  readings above say they will be; the first sweep after this upgrade is the
  first reading of them in a store.
- The walk past a hundred, by the collector against GitHub. This account has
  33, 15, 11, 24 and 19 items in the five states, so its sweeps still read a
  page each. The pages were read with `gh` on other accounts, and the walk is
  held to them by unit tests and by the binary against the fake GitHub.
- The thousand is reported, not worked around. Splitting a search by
  `created:` ranges would reach past it and was not done, and the warning has
  fired against the fake GitHub only.
- An open state is walked by an offset cursor, so an item that leaves it
  while a walk of more than one page is under way moves every later item up
  by one, and one of them can miss that sweep and be read by the next. That
  is read from the cursor, not seen.
- That Grafana renders `${repo:text}` as "All" when All is selected, which the
  SQL stores' filter rests on, is read from `@grafana/scenes` and from
  `templateSrv` before it, not seen in a browser: the checkers and the
  containerised suite render the variables themselves. A Grafana that
  answered anything else would leave the archived rows out under All, as
  2.5.1 did, and count nothing twice.
- The Prometheus and Graphite sums were checked by evaluators of their query
  languages written for the test, over a made-up account with two owners'
  `.github`, a repository archived while the collector ran and one set aside
  after a backfill. The containerised suite runs them against the real
  stores, but only InfluxDB has counted an archived repository's stars
  there.
- Twenty five archived repositories to a query was measured on the heaviest
  of two organisations. A heavier batch meets the gateway's 502, which halves
  it and retries, so it costs a query and never a repository, but no account
  has been seen needing it.

## 2.5.1 - 2026-09-25

What releasing and deploying 2.5.0 turned up, fixed the same day: a publish
that gave up over one panel, release notes nobody could read, images nobody
had signed, and a handful of smaller things that said less than they should.

- **Publishing no longer gives up over the Loki datasource.** Since 2.4.0, a
  Grafana token that may publish dashboards and may not create datasources,
  the Editor the documentation recommends, abandoned every dashboard when a
  Loki sink was configured without `grafana.datasource.loki_uid`. The
  publisher looked for its own `ghchronicle-loki`, found none, was refused
  making it, and returned that refusal as the run's error, although Loki
  feeds one panel, the failed job output. In production the run said "could
  not publish the dashboard, carrying on without it" and published nothing.
  It now warns once, naming `ghchronicle-loki`, the permission the token
  lacks, `grafana.datasource.loki_uid`, and up to three Loki datasources
  Grafana already has with their addresses, and publishes every dashboard
  with that panel left as its note. The warning speaks for that panel alone,
  since it is printed before any store is tried. A `ghchronicle-loki` an
  earlier run made, which the token may read and may not rewrite, is read as
  it is, with a warning naming `datasources:write` and `loki_uid`. A sink
  with a `tenant_id` asks for that rewrite on every start, and so does a
  datasource whose address was changed by hand, so an Admin run followed by
  a token scoped down to an Editor met the refusal on every start after it.
- **A Loki datasource Grafana already has at the sink's address is adopted.**
  Before making `ghchronicle-loki`, the publisher lists what Grafana has and
  adopts a Loki datasource at the address it works out from the sink, which
  needs only `datasources:read` and writes nothing. Never for a sink with a
  `tenant_id`: the tenant travels in a secret Grafana does not hand back, so
  a datasource there could be reading another tenant. Store datasources are
  still made rather than adopted by address, because one InfluxDB, PostgreSQL
  or Elasticsearch serves many databases under credentials Grafana never
  shows, and an address does not say which of them a datasource can see. A
  health probe the token may not make now names `datasources:query` instead
  of advising `grafana.datasource.url`, a refused store datasource names the
  permission and `grafana.datasource.uid`, and a warning from a publish on
  start reaches the journal as a warning.
- **`-uninstall dashboard` removes `ghchronicle-loki`.** It removed only the
  datasources named after a store, so the Loki one stayed while the
  dashboards page said a datasource it created goes. One named in
  `loki_uid`, and one it adopted, are left, like any datasource it did not
  make.
- **The release page carries this file's section for the version** instead
  of GoReleaser's list of commits. That list filed all ten 2.5.0 commits
  under "Other", sorted alphabetically, each behind a forty-character SHA,
  because its groups expected conventional prefixes this repository does not
  use, and the section written for the release never reached its page. The
  preflight job now cuts the `## X.Y.Z - YYYY-MM-DD` section out and refuses
  a tag without one, with an empty one, or with a relative link, written
  inline, as a reference definition or in an HTML `href` or `src`, which the
  release page would resolve against `/releases/tag/`. GoReleaser publishes
  it above the pull lines, the verification commands and the compare link,
  which is where the commits still are.
- **The container images are signed, and `latest` waits for the check.** On
  ghcr.io and on Docker Hub, with cosign, keylessly, by the same release
  workflow identity as the checksum file, and the release job verifies each
  image against that identity at its exact tag before it runs it, so an
  unsigned image now fails the release. GoReleaser pushes only the version
  tag. `latest`, which every documented `docker run` pulls, is moved onto the
  same digest by the release job once both checks pass, so a release that
  fails leaves it on the previous one rather than on an image nobody
  verified; pushed with the version tag, it moved before the signing. Every
  image up to 2.5.0 is unsigned.
  [Verify the image](https://jmrp.io/docs/ghchronicle/install/docker/#verify-the-image)
  has the command, which needs cosign 3: cosign 2.6.1 finds the signature
  only with `--new-bundle-format`, cosign 2.5.0 not even then, and the
  release page's footer now says so too.
- **The Action takes `version` with or without its v.** `version: 2.5.0` went
  into the download URL as typed and failed with a 404, while `v2.5.0`
  worked. Both now install the same release. The major tag `v2` is refused by
  name before anything is fetched, since it is what `uses:` takes and no
  binaries hang from it, and a release that does not exist is reported as
  missing rather than as a gzip error.
- **`install.ps1` checks the signature too.** It verified the checksum and
  stopped, while `install.sh` also verifies `checksums.txt` with cosign when
  cosign is on PATH. It now does the same, with the identity and issuer
  `install.sh` uses, which a test holds the two scripts to, and both
  installers say when only the checksum was verified. Neither takes an old
  or unreachable cosign for a forged file any more. A cosign older than
  2.4.2 cannot read the bundle every release publishes and fails exactly as
  it would on a forged file, which `install.sh` reported as "Do not use what
  was downloaded"; both installers now say that cosign is too old and go on
  on the checksum, as they do without one. A newer cosign that fails still
  stops the install, and the refusal now quotes it, so "tuf refresh failed"
  from a machine that cannot reach Sigstore reads as what it is.
- **`install.sh` no longer ends with "/dev/tty: No such device or address"**
  when it runs without a controlling terminal: a CI job, a container build, a
  provisioning run. The probe's stderr redirection was made after the open it
  was meant to silence; it now covers the open.
- **A `go install` build says what it is.** It printed
  `ghchronicle 2.5.0 (commit unknown, built unknown)`, because a module
  download carries no checkout and so no commit or date. It now prints the
  module version the go command fetched and the Go release that compiled it,
  `ghchronicle 2.5.1 (module v2.5.1, built with go1.27.1)`. Every other
  build prints the line it printed before, byte for byte.
- **A Prometheus or OTLP mean is over the items that carried the field.** The
  count reduction divided every summed field by the number of points in the
  series, so a field a collector leaves out when it has no honest value
  counted as a zero there. `steps` on a job GitHub no longer serves steps
  for, which 2.5.0 stopped writing, and `queued_seconds` on a job with no
  start time pulled `github_workflow_jobs_steps_mean` and
  `github_workflow_jobs_queued_seconds_mean` down, as the 2.5.0 entry said,
  and so did every other field written only when it has a value, the wait
  for a first human review among them. Averaging over the items that carried
  the field is what AVG does with a null in the SQL stores. Three
  fields whose absence is itself the answer are still averaged over every
  item, so their means stay shares: `merged` on an outside contribution,
  `advanced` on a fork and `pull_requests` on a workflow run.
- **A first stargazer walk that fails is walked whole again, and a backfill
  walks every list whole.** The state file recorded a repository under
  `first_saw` before its one full walk of the stargazer list, so a first walk
  a 502 cut short retired the repository all the same: every later sweep read
  only its newest hundred, through the batch, and the older stars stayed
  unread for good, because a backfill read a recorded list by page one and
  the last page too. It is recorded now only once the walk came back without
  an error, as `history_read` already was, and a backfill walks every
  stargazer list whole, recorded or not, so running one recovers a repository
  an earlier release recorded after a walk that failed.
- **Every HTTP client has a connection pool of its own.** The Grafana client,
  the uninstall's store calls, the guided setup's probe and the containerised
  suite's harness sent through `http.DefaultClient`, whose pool a parallel
  test closing its httptest server empties under a request another test has
  in flight. The source test that holds every `http.Client` literal to a
  transport of its own now also refuses `http.DefaultClient` and the
  `http.Get`, `Head`, `Post` and `PostForm` that send through it, which is
  how these four went unnoticed.
- **Two checks of the containerised suite no longer depend on the clock.** The
  InfluxDB check that a second sweep adds no dated row counted `gh_star_day`
  whole, and a second sweep after a UTC midnight writes the new day by
  design; it now counts the days before the first sweep's own. The same
  midnight also moved the other four tables it counts: each sweep had a fake
  GitHub of its own on the live clock, so the second one dated every traffic
  day, run, commit and star in the fixtures a day later, onto timestamps the
  first never wrote. The second sweep's fake is now frozen at the first
  sweep's start. The PostgreSQL star-day check loads one sweep and compares
  exact sets, which a midnight cannot move, and now says so.
- **The release run ends by linking the Marketplace box.** Listing a
  release in the Marketplace's version menu takes a tick on its edit page,
  and no token can give it: GitHub asks for a 2FA confirmation only the web
  page performs, and the releases API has no parameter for it. 2.5.0 stayed
  out of the menu until it was ticked by hand, so the last job of the release
  run now leaves a notice and a line in the run's summary with that page's
  address.
- **Two things this file and the site said that were not so.** The 2.4.0
  entry had `-setup` serving whoever arrived through Homebrew, and there is
  no Homebrew channel; it now says a tarball. The stars screenshot's
  description on the panels page described the picture it replaced.
  `.github/ACTION.md` now also answers why the Marketplace listing offered
  only `v1`: no setting names a major version, and the listing's version
  menu is the releases ticked for the Marketplace one by one, which no
  numbered release had been.

Measured against a throwaway Grafana 13.2.1, with an Editor service account, a
Prometheus datasource named in `grafana.datasource.uid`, and a Loki sink at an
address Grafana has no datasource for: 2.5.0 ended with "Permissions needed:
datasources:create" and exit 1, and this release warned, named the Loki
datasource Grafana already had and its address, published the dashboard with
the panel as text, and exited 0. With the sink at the address of that
datasource it adopted it, the panel read it, and no `ghchronicle-loki` was
made. A service account with no role is refused `datasources:read` on the list
and `datasources:query` on the probe, and the run now ends naming the second.
A new case of the containerised suite mints an Editor on its own Grafana and
holds the publish to three outcomes: the note, the adoption, and a
`ghchronicle-loki` an Admin made with a tenant, read as it is. On the same
Grafana 13.2.1, a `ghchronicle-loki` an Admin token had made, for a sink with
a `tenant_id` and then again with its address changed by hand in Grafana, was
read as it was by an Editor: one warning naming `datasources:write`, exit 0,
and the panel drawing logs from it. With an Editor, a Loki sink and an
InfluxDB sink but no `grafana.datasource.uid`, the Loki warning is followed by
the store's refusal and exit 1, and no longer promises every dashboard first.

Measured with GoReleaser 2.18.1, in a full release of the 2.5.0 tree against a
local registry and a stand-in for the GitHub API: the body posted was the
2.5.0 section byte for byte above the footer, with no list of commits, and the
images were signed by digest after the push and before the release was
created, two signatures per digest while `latest` was still one of
GoReleaser's tags, which cosign 3.1.3 verifies and cosign 2.6.1 finds only
with `--new-bundle-format`. Moving `latest` from the version tag with
`docker buildx imagetools create`, as the release job now does, was measured
with buildx 0.37.0 against a local registry on a two-platform OCI index carrying
index annotations, as GoReleaser pushes one: `latest` came out under the same
digest, which is what lets the version tag's signature cover it.
`changelog.disable: true`, the tidier-looking setting, published the footer
and nothing above it, which is why the configuration keeps it false. The
preflight's cut of the 2.5.0 section matches its lines in this file under mawk
and gawk, and it refuses a missing section, an empty one and a relative link.
Under GNU grep 3.11 it refuses a relative inline link, reference definition,
`href` and `src`, and a bare `#fragment`, which points into this file and not
at the release page, and it lets through absolute and `mailto:` links and a
footnote definition. Against the published 2.5.0 release with cosign 3.1.3,
both installers answered "signature verified with cosign", and both refused a
copy whose `checksums.txt` had one line added, installing nothing. With cosign
2.4.0, which exits 1 on that bundle where 2.4.2 verifies it, both installed
2.5.0 on the checksum and said the cosign was too old to read the signature,
where `install.sh` had refused it; with cosign 3.1.3 and Sigstore's TUF CDN
unreachable, `install.sh` refused and quoted "tuf refresh failed". The
Action's install step, run as written against GitHub, installed 2.5.0 from
`2.5.0`, `v2.5.0` and `latest`, refused `v2`, and said there is no archive for
`2.9.9`. `go install` of v2.5.0 through the public proxy printed the unknowns,
a file proxy serving this tree as v2.5.1 printed the module line, and a
GoReleaser-stamped build, `make build`, the Dockerfile with and without its
build arguments and an unstamped build in a checkout print what they printed
before.

Not verified, and worth saying plainly:

- The keyless signing of an image, and the move of `latest` inside the
  release job. The first needs the Actions OIDC token, so the local release
  signed with a key; the first proof of both is this release's own job,
  which verifies each image before it runs it and checks the digest `latest`
  lands on.
- The production Grafana. It was fixed with `loki_uid` before any of this
  was written, and the warning and the adoption were measured on a throwaway
  one.
- A token that may create datasources still makes `ghchronicle-loki` at the
  sink's push address with the path dropped, which, where the collector
  pushes to a published port and Grafana reaches Loki on a container network,
  is an address Grafana may not reach, and nothing asks the Loki datasource
  whether it answers. `loki_uid` is the answer there.
- `install.ps1` ran under PowerShell 7.6.6 on Linux, not under Windows
  PowerShell 5.1, so its handling there of what cosign writes to stderr is
  reasoned rather than seen.
- The Marketplace. That ticking the box on a numbered release puts it in the
  listing's version menu is GitHub's documented behaviour; nobody has ticked
  one yet.
- How far the new divisor moves the Prometheus panels that read these means
  on this account. The unit tests pin the divisor; no exporter was read
  before and after.
- A backfill walking a recorded stargazer list whole, against GitHub. A unit
  test holds it to every page of a five-page list, where it read two; no
  backfill has run with it, and its cost is one request per hundred stars,
  the same as a repository's first sweep.
- The containerised InfluxDB check across a real UTC midnight. It passed with
  the second sweep's fake frozen, and the dates a frozen fake resolves do not
  depend on the hour, but no run has straddled one.

## 2.5.0 - 2026-09-25

`go install` reaches this release, the first one it can, and the stars of a
repository whose stargazers GitHub will not list are counted all the same.

- **The module path is `github.com/jmrplens/ghchronicle/v2`.** Every v2 tag
  from 2.0.0 to 2.4.0 carried a `go.mod` that said
  `github.com/jmrplens/ghchronicle`, and Go refuses a v2 or later tag whose
  path does not end in its major. The module mirror listed v1.0.0 and nothing
  else, and answered v2.4.0 with "module path must match major version", so
  the `go install ...@latest` on the home page, in the README and on every
  install page installed 1.0.0, the first public release, and said nothing.
  The command is now
  `go install github.com/jmrplens/ghchronicle/v2/cmd/ghchronicle@latest`.
  The old path still resolves, to 1.0.0, because a tag already pushed keeps
  the `go.mod` it was cut from; nothing can move it.
- **The release preflight holds the tag's major to the module path**, beside
  the VERSION check it already made, so a v3 tag with a v2 path is refused
  before anything is built.
- **`gh_star_day`, every repository's stars by day.** A new measurement in the
  `stars` family, read from `/repos/{owner}/{repo}/stargazers/history` for
  every repository a sweep collects: one row per repository and day, tagged
  `full_name`, `owner` and `repo`, with that day's stars in `stars`. Every
  repository, because since July 2026 GitHub serves the stargazer list only to
  a repository's admins and collaborators. Anyone else gets a 404 from REST and
  an empty list from GraphQL, which counts 46,395 stars on cli/cli and lists
  none of them, so a repository where the account is neither has written no
  new `gh_star` row since, and the two panels that count stars over time left
  it out. The history is served to anyone who can read the repository, so
  reading it for all of them needs nothing detected and nothing configured. It
  went unread until now because it was taken to hold only the last thirty
  weeks: that is its page size, and it pages back to the week the repository
  was created. `gh_star` stays, as the record of who starred and when to the
  second, wherever GitHub still lists them.
- **A day is GitHub's calendar day in Los Angeles.** GitHub labels each week
  Sunday 00:00 UTC but buckets its days by the calendar in
  America/Los_Angeles, daylight saving included. Each row is stamped at 00:00
  UTC of that date, the convention `gh_contribution_day` already follows, and
  anchored to GitHub's week rather than the clock, so a Tuesday sweep and a
  Friday one land on the same rows. No day after the sweep is written: GitHub
  fills the rest of the current week with zeros, and those are left out.
- **An unstar changes the past.** The history is the stargazers GitHub lists
  today, each counted on the day they starred, so an unstar takes a star off
  the day it was given, not the day it was withdrawn. Page one, the newest
  thirty weeks, is what every sweep reads, and it is written whole, zero days
  included, so an unstar there is applied. Older pages are read on a first
  walk and in a backfill and write only the days that have stars, so an older
  day that loses its only star keeps it, the way `gh_star` keeps every star it
  ever saw. Zeros stop at page one because InfluxDB 3 Core writes a file for
  every day a write touches and never compacts them, and zeros back to each
  repository's creation would have been thousands of files a backfill.
- **Two panels change meaning.** Stars gained over time counts `gh_star_day`
  in every store but Prometheus, and Stars over time climbs from it in
  InfluxDB and PostgreSQL; in Graphite and Elasticsearch that curve stays on
  the `gh_repo` snapshots it already drew. Where they read the history now,
  they counted `gh_star` rows before, and what they show moves in three ways.
  The bars are Pacific days, so a star given on a European morning can sit a
  day before the date Recent stars shows for it. The curve counts the
  stargazers GitHub lists when the history is read, so it has lost every star
  taken back before that first read, which the `gh_star` curve kept. It
  usually sits at or a little below the `gh_repo` count, which also counts
  accounts GitHub no longer lists: 25 against 26 on A-Lab, 7 against 8 on
  LoVE-BASS, 12 against 13 on mastodon_official_profiles. A star given more
  than thirty weeks ago and taken back after that read stays in it: only a
  backfill that reaches back to its day lowers that day, and only while the
  day holds another star, so one that was its day's only star stays for good
  and the curve can also sit above the `gh_repo` count. And both panels
  now hold the repositories whose list is hidden, which have stars there and
  no names under Recent stars, since names exist only in `gh_star`. No panel
  counts from both measurements, so no star is counted twice. Prometheus is
  unchanged: `gh_star_day` is not reduced for it, because a per-day history
  has no current value and an unstar lowering a past day would read as a
  counter reset, so its Stars gained still counts only the lists the token can
  read. Graphite's stand-in for Recent stars, a count per repository because
  Graphite keeps no names, reads the history as well.
- **It costs a request per repository a sweep, and usually nothing.** A sweep
  reads page one, which GitHub answers 304 unless a day of the last thirty
  weeks gained or lost a star or a new week began, and a 304 is not charged.
  The first sweep after upgrading reads each repository's history whole, once:
  a page per thirty weeks of its life, 180 pages for the 19 starred
  repositories of this account. The state file records that under
  `history_read`, set only after a walk has reached the end of the history, so
  a walk cut short is read again by the next sweep rather than left partial
  until a backfill: by a 502, and also by a later page answering 403 or 404,
  which is a secondary limit sent without its headers or a repository gone
  mid-walk, not the end of the history. What it does cost is time: a REST
  round trip per repository every sweep, which the GraphQL batch had removed
  for the lists.
- **GitHub Enterprise Server has no star history.** Its REST API, checked
  against 3.21 and 3.22, serves the stargazer list and not
  `stargazers/history`, so against it every repository answers 404, which is
  read as nothing to collect, and `gh_star_day` stays empty. The panels that
  count stars from it are empty with it: Stars gained over time and Stars over
  time in InfluxDB and the SQL stores, Stars gained over time in Graphite and
  Elasticsearch, and Graphite's Recent stars, all of which drew from `gh_star`
  before and showed stars there. `gh_star` is still collected, Recent stars
  still lists names from it in the other stores, and Prometheus still counts
  from it. A history that answered 404 is not recorded as read, so a server
  that starts serving it has each repository's history read whole on the
  next sweep.
- **A job whose steps GitHub no longer serves carries no `steps`.** GitHub
  keeps a run's jobs much longer than their steps. Every job of a run created
  before about 12 April came back with its times, its runner and its
  conclusion over an empty step list, and every job of a later run with its
  steps, so those jobs were written with `steps` at 0, a count nobody made, as
  far back as a backfill reached. A job that ended as success, failure or
  timed out and lists no steps is now written without the field: it ran, so
  it had steps, and in InfluxDB, the SQL stores and Elasticsearch anything that
  averages steps per job now averages the jobs that reported them. The
  Prometheus and OTLP `github_workflow_jobs_steps_mean` still divides by every
  job in the batch, as it already does for `queued_seconds`, so a batch that
  mixes old and new runs reads low there as it did before. A backfill that
  re-reads a job it saw when the run was new no longer writes a 0 over the
  count it wrote then. A skipped job keeps
  its 0, because it runs no steps, and so does a cancelled one, which may have
  stopped before its first. The zeros already stored stay: InfluxDB merges
  the fields of a point written again and the SQL upsert sets only the
  columns a point carries, so collecting a job again leaves its old 0, which
  in PostgreSQL
  `UPDATE gh_workflow_job SET steps = NULL WHERE steps = 0 AND conclusion IN ('success', 'failure', 'timed_out')`
  clears. Elasticsearch replaces a document whole, so there a job collected
  again loses its 0, and with it any count it was first written with.
- **A walk whose first page is answered from the ETag cache reads its
  second.** GitHub sends no Link header with a 304, and the client handed that
  missing header back as it came. Three walks find their next page there, the
  stargazer list by page number and the activity log and the Dependabot alerts
  by cursor, so any of them whose first page came from the cache ended after
  it, without an error. The cache now keeps the Link header beside the body and
  replays it with the 304. An ordinary sweep lost nothing to it: it reads the
  stargazer list only on first sight, one page of Dependabot alerts, and a
  second page of the activity log that has nothing new when the first has not
  changed. What it could cut short was a second request for the same first
  page in one process, which is what a `-backfill-retry` pass makes when it
  goes back for a walk that failed partway: that walk stopped at page one and
  was recorded as done.

Measured on a copy of the repository served to Go as its origin, with three
tags: the old path resolves `@latest` to v1.0.0 and refuses v2.98.0 with the
mirror's own error, and the new one resolves `@latest` to v2.99.0, builds, and
the binary reports that number.

Measured on this account on 24 and 25 September, against the full REST
stargazer list of each of its 19 starred repositories, 440 stars: the history
sums to the length of the list in all 19. Bucketed by Los Angeles days the two
agree on every star. Bucketed by UTC days, 38 of the 127 days compared on
phonometry disagree, and by a fixed UTC-7 one winter star does, which is what
says daylight saving is part of it. On the same endpoint a page is thirty
weeks, `per_page`'s default and its cap (a smaller one is honoured), the 200
for page one carries `rel="next"` and
`rel="last"`, and the 304 for it carries no Link at all and is not charged:
`x-ratelimit-used` read 297 before and after. The steps cutoff was read on 24
September across this account's runs, and a skipped job was measured listing
none.

Not verified, and worth saying plainly:

- The public mirror and pkg.go.dev picking up the first `/v2` tag, which the
  copy could not show. RELEASING.md now says how to check both after tagging.
- The day bucketing is measured, not documented. GitHub documents neither the
  Pacific days nor a week label that does not match them. If it moves to UTC
  days, a star shifts by at most a day and nothing breaks.
- Nobody has watched an unstar arrive. That it rewrites the day the star was
  given is read from the history always summing to the list GitHub serves
  today; none of these repositories lost a star while they were measured, and
  starring then unstarring to watch would have been a write.
- The steps cutoff is one snapshot. It cannot say whether GitHub keeps steps
  for a window, about five and a half months that day, that moves with the
  calendar, or dropped them before a fixed date; a second reading some weeks
  later would.
- The backfill the missing Link could cut short is read from the code. No
  store was searched for a walk it had stopped.
- That GitHub Enterprise Server has no star history is read from its REST
  documentation and its OpenAPI descriptions for 3.21 and 3.22, not from a
  running server.

## 2.4.0 - 2026-09-21

Installing this stopped being a reading exercise.

- **`ghchronicle -setup`**, a guided install that lives in the binary rather
  than in the install script, so it serves whoever arrived through a tarball,
  Docker or a zip on Windows just as well as whoever piped `install.sh` into a
  shell. It asks for a GitHub token, which account to collect, where to keep the
  data, whether to publish the dashboard, and whether to install a service, and
  it checks every answer against the real thing before writing anything: the
  token against `/user`, the store and Grafana against their own probes. The
  configuration it writes carries only `${VAR}` references; the credentials go
  beside it in a file only the account that ran it can read. It never replaces
  an existing service without asking.
- **Five compose files that come up on their own**, one per combination of
  store and Grafana, each carrying its own configuration inline so there is no
  second file to write, and each reachable from a picker on the Docker page.
  With Grafana in the stack the dashboard is already there: the collector
  publishes it at start-up and points it at the store beside it.
- **PostgreSQL as a connecting sink**, through pgx, beside the SQL file it
  could already write. With it, all five stores can now derive their own
  Grafana datasource.
- **The documentation reorganised by audience**: what somebody using the tool
  needs, what they look up, and what only matters to whoever edits the
  repository. The README became the front door, because it is what a reader
  sees first.

Measured here, on this server: every compose combination brought up against
the real images, with the dashboard drawing real data, not merely a container
that starts.

Not verified, and worth saying plainly: `-setup` has only ever been run
end to end on Linux. The launchd agent and the Windows scheduled task are built
and read by tests that run on all three systems in CI, and the code that writes
them is exercised there, but nobody has installed either on a real machine.

## 2.3.0 - 2026-09-19

- **The binary publishes its own dashboard.** Given a Grafana URL and a token,
  `ghchronicle -publish-dashboard` creates the datasource if it is missing and
  publishes the dashboard for every store it writes to, and the collector does
  the same at start-up. Importing JSON by hand became one of the ways in rather
  than the only one.
- A dashboard left behind by a store that is no longer configured is **named**
  rather than silently overwritten or silently kept.

## 2.2.0 - 2026-09-19

- **The version is stamped at build time** instead of being kept in step by
  hand in several files. A check fails the build when the places that mention
  it disagree.
- The installer says what it does now rather than what it used to do, and
  **warns on all three systems** when the binary it just installed is not the
  one the shell will find first.

## 2.1.0 - 2026-09-19

- **Install in one line**, and the installed binary behaves as a command on
  Linux, macOS and Windows.
- **The backfill can be watched, and goes back on its own.**
- Every HTTP client got **its own connection pool**. Sharing one meant a slow
  store could hold up the collector.

## 2.0.0 - 2026-09-18

The release that made a long backfill survivable.

- **A backfill that a kill interrupts is picked up where it left off.** The
  checkpoint covers the sinks in scope and carries no credentials.
- **What a collector gathered before it failed is kept**, and what failed is
  said, rather than the whole sweep being lost to one bad family.
- **A repository is named the same way in every measurement**, which a
  dashboard cannot work around once it is not.
- Every panel of every dashboard was drawn against a real account; three that
  the pictures showed to be wrong were fixed.

## 1.0.0 - 2026-09-15

First public release. MIT, one initial commit, twenty release artifacts with
SBOMs and keyless cosign signatures, images on ghcr.io and Docker Hub for amd64
and arm64, and the bilingual documentation on GitHub Pages.
