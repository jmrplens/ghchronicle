# Publishing the Action

The Action lives in this repository, not in one of its own. `action.yml` sits
at the repository root, which is the only thing GitHub requires, and a workflow
elsewhere reaches it as:

```yaml
- uses: jmrplens/ghchronicle@v2
```

## Why not a separate repository

A separate `ghchronicle-action` repository would mean two release cycles for
one tool, a second place for the documentation to drift, and a version number
on the Action that says nothing about which collector it runs. The Action is a
thin wrapper: it downloads a release binary of this repository and calls it.
Tying it to the same tag is the property that makes it predictable.

The one reason to split would be an Action whose inputs need to move
independently of the tool. That is not this.

## Listing it in the Marketplace

GitHub's Marketplace takes an Action from any public repository whose
`action.yml` is at the root, so this repository qualifies as it stands.

1. Push a tag (`vX.Y.Z`). The release workflow builds the binaries the Action
    downloads.
2. Open the release on GitHub. It offers "Publish this Action to the GitHub
    Marketplace"; tick it, accept the terms, and choose the categories
    (Monitoring, and Utilities).
3. Nothing: the release workflow moves the major tag so `@v2` keeps
    resolving, in its last job, once the release has published.

    Every Action in the Marketplace keeps such a tag. A workflow pinned to
    `@v2` follows the patch releases without editing, because a `uses:` ref is
    an exact git lookup and not a semver range: there is no resolution from
    `v2` to the newest `v2.x.y`, which is why the pointer has to exist and has
    to move.

The listing name, description, icon and colour come from the `name`,
`description` and `branding` keys of `action.yml`, and it takes them from the
`action.yml` of the newest published release rather than from `main`: a
change to them shows on the listing at the next tag, not at the next push.

The tile cannot be the project's own mark. GitHub draws an Action's tile from
one Feather icon in one of nine colours and takes no image, so `grid` on
`green` is the nearest thing in that set to a green grid of squares.

## The token

The automatic `GITHUB_TOKEN` is not enough, and it is worth being specific
about why rather than letting someone discover it as an empty dashboard:

- **Traffic** (views, clones, referrers, paths) is only shown to a caller who
  could push to the repository. The automatic token has that for the repository
  the workflow runs in, and nothing else.
- **Dependabot and code scanning alerts** need `security_events`.
- **Packages** need `read:packages`.
- Everything account-wide (followers, contributions, billing, notifications,
  the stars you gave) is about the user, not about a repository, and the
  automatic token is not a user.

So: create a personal access token, store it as a repository secret, and pass
it as the `token` input. A classic token needs `repo`, `read:user`,
`read:org`, `read:packages` and `security_events`. A fine-grained token needs
read access to the repositories plus the account permissions for followers,
gists, packages and plan.

A token is not needed at all if the only thing wanted is a card of public
numbers, but the traffic and alert panels will be empty, and the log will say
`not available (403)` for each, which is the collector reporting a permission
rather than a failure.
