# Demoing the content editor MCP server

`cmd/mcp` is the MCP server from
[the content editor design](../docs/superpowers/specs/2026-09-19-content-editor-mcp-design.md).
The media person's own Claude does the editing; this process is hands and
guardrails. It runs `hugo` and `git`, and it enforces the path fence, the
`media/<user>/` branch namespace and commit authorship.

The demo runs it with one fixed bearer token and no Keycloak, so the whole
OAuth leg is skipped. Everything else is real: a real clone, real worktrees, a
real Hugo build, real commits.

## Reset

Worktrees live on the state volume and outlive the process on purpose, so a
rehearsal leaves the next run continuing the rehearsal's branch and starting
from its already-edited colour. Wipe both before demoing, and prune the
branches a rehearsal pushed to the origin repository:

```bash
export PATH=$HOME/.local/bin:$PATH

devctl stop mcp
rm -rf $HOME/mcp-state/repo $HOME/mcp-state/state
cd /home/rvirt/code/prodeko-hack/analytics
git worktree prune
git for-each-ref --format='%(refname:short)' refs/heads/media/ | xargs -r git branch -D
```

## Quickstart

```bash
export PATH=$HOME/.local/bin:$PATH

git clone /home/rvirt/code/prodeko-hack/analytics $HOME/mcp-state/repo
cd /home/rvirt/code/prodeko-hack/analytics/proxy
go build -o $HOME/mcp-state/mcp ./cmd/mcp

devctl start mcp -- env \
  LISTEN_ADDR=0.0.0.0:8093 \
  PUBLIC_URL=http://192.168.0.2:8093 \
  MCP_REPO_PATH=$HOME/mcp-state/repo \
  MCP_STATE_DIR=$HOME/mcp-state/state \
  MCP_DEV_BEARER=demo-bearer-prodeko \
  SESSION_SECRET=$(head -c32 /dev/urandom | base64) \
  GIT_COMMITTER_NAME='Prodeko Media Bot' \
  GIT_COMMITTER_EMAIL=media-bot@prodeko.org \
  $HOME/mcp-state/mcp
```

The clone's origin is the working repository rather than a bare mirror, which
is the point: with no GitHub token, `submit` pushes to that origin, so the
`media/*` branches it creates are inspectable with plain `git` and never touch
the checked-out branch.

`screenshot` runs a headless Chromium. The server image ships one; a server run
straight off the VM like this uses whatever is on `PATH`, or the one
`MCP_CHROMIUM_BIN` names. Without a browser, that one tool refuses every call
naming what it looked for, the startup log warns about it, and every other tool
works as it is.

`devctl logs -f mcp` follows the server. Every `tools/call` is logged with the
tool name, the user and how long it took; that log is the audit trail for media
edits.

Check it is up:

```bash
curl -s localhost:8093/healthz
curl -s localhost:8093/.well-known/oauth-authorization-server | jq
```

## Connecting Claude Code

```bash
claude mcp add --transport http prodeko-editor http://192.168.0.2:8093/mcp \
  --header "Authorization: Bearer demo-bearer-prodeko"
```

`192.168.0.2` is this VM on the host network. From the VM itself `localhost`
works just as well.

`MCP_DEV_BEARER` is one fixed string that authenticates as
`dev-editor / Dev Editor / dev@prodeko.org`, bypassing Keycloak and the role
conjunction entirely. It exists for this demo and must never be set in
production; the server logs a warning at startup whenever it is.

## The demo script

Ask Claude for a colour change and watch it work:

> The "perinteinen" light blue on prodeko.org is a bit dull. Find where it is
> defined and make it brighter, then build and submit the change.

What should happen, in one turn: `search` or `list_files` to find
`site/assets/css/tokens/colors.css`, a ranged `read_file`, an `edit_file`
replacing the hex value, `build` coming back green in about half a second, and
`submit` returning a branch name and a diffstat.

Then inspect what it actually did:

```bash
git -C $HOME/mcp-state/repo branch -a | grep media
git -C $HOME/mcp-state/repo log -1 --format='%an <%ae> / committer %cn' \
  origin/media/dev-editor/<slug>
git -C $HOME/mcp-state/repo show origin/media/dev-editor/<slug>
```

The author is the signed-in identity and the committer is the bot, which is
what makes media commits distinguishable from Decap commits in history.

Four things are worth demonstrating because they are the safety story rather
than the feature:

- Ask it to edit `.github/workflows/preview.yml`. The fence refuses and names
  the rule. A media-authored workflow edit would be a direct path to the VM.
- Ask it to edit `site/layouts/baseof.html` or `site/layouts/partials/head.html`.
  The partials and the page layouts are writable; the page skeleton, the asset
  pipeline templates and the shortcodes are not, and the refusal names which
  group the file is in and why. `get_conventions` states the whole list.
- Edit a partial and ask it to submit without looking at the result. `submit`
  refuses, names `screenshot`, and asks for the sentence about what looks
  different that a maintainer reads before the diff.
- Ask it to break a shortcode *call* in a page — `site/content/fi/tapahtumat.md`
  has several — and build. Hugo's error comes back verbatim in the same turn,
  which is what makes the loop converge instead of flail. The shortcodes
  themselves are under `site/layouts/`, so asking it to break one of those is
  the fence's refusal, not a build error.

Asking for a second change continues the same branch. That survives a server
restart, because the worktree on disk is what records which change someone is
in the middle of.

## Real pull requests

Set both GitHub variables and `submit` stops being a dry run: it pushes to
GitHub, opens a draft pull request labelled `media`, and returns the number
with a `https://pr-<N>.preview.prodeko.org/` link.

```
GITHUB_TOKEN=<fine-grained token>
GITHUB_REPO=prodeko/prodeko-org
```

The token wants Contents and Pull requests, read and write, on that repository
alone. Point `MCP_REPO_PATH` at a clone whose `origin` is the GitHub
repository, or the push has nowhere to go. There is no merge tool, and the real
boundary is branch protection on `main` rather than this server's politeness.

## Not wired yet

- OAuth end to end against claude.ai. The authorization server is complete and
  tested — discovery, permissive dynamic registration, PKCE S256, the Keycloak
  leg, the `prodeko-org-media` + `membership` conjunction, sealed 8-hour access
  tokens — but it has never met a real connector, and the design names that as
  the spike that can still kill the feature. The demo runs on
  `MCP_DEV_BEARER` instead. With `KEYCLOAK_ISSUER` unset there is nowhere to
  sign in and `/authorize` answers `temporarily_unavailable`.
- No refresh grant. An access token lasts eight hours and then the connector
  signs in again.
- The infrastructure deploy. The `prodeko_mcp` Ansible role, the `edit` DNS
  record and the Caddy site block live in infra-prodeko and have not been
  applied; the container image has a placeholder digest.
- The Keycloak side is hand work that has not been done: the confidential
  client and the `prodeko-org-media` realm role in the production admin
  console.
- Images. The model has no file bytes to give `write_file`, so image uploads
  stay in Decap.
