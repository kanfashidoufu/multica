# Version branch decisions and checkout

## Evidence order

A target is a tuple: **bug + version + repository URL + exact remote branch**.
Use the strongest applicable evidence; contradictory current evidence always
needs human resolution.

1. A current issue-creator reply naming that tuple is authoritative. Reuse a
   prior confirmation for the same tuple; do not ask again just because the run
   restarted or a metadata cursor disappeared. A reply saying "yes" is valid
   only in response to a proposal containing exactly one repo/branch choice.
2. An explicit maintained project/repository version-to-branch mapping can
   resolve the tuple automatically when it names this exact bug version and
   repository, the branch exists remotely, and no current comment overrides or
   contradicts it. Record the mapping source/text. A resource's `ref` without an
   explicit connection to the bug version is a checkout default, not a mapping;
   `default_branch_hint`, a tag/SHA, HEAD and a current environment branch are
   not release-branch evidence.
3. Discover candidates by querying the actual remote. Fuzzy name matching
   supplies options for a person, never an automatic branch decision by count.

```bash
bash <this-skill-dir>/scripts/find-version-branches.sh "<bug_version_name>" "<checkout-path>"
```

The script reads `git ls-remote --heads origin`, not cached remote-tracking refs.
It prints `version_token`, `match_count`, and `branch` lines. It requires one
complete dotted version and rejects ambiguous inputs. For `2.91.56`, branches
such as `release/v2.91.56_wn`, `release/v2.91.56_merge`, and `v2.91.56` are
candidates. `2.91.560`, `2.91.56.1`, and `12.91.56` are different versions.
Remote/auth failure is a discovery failure, not zero matches.

Never choose between `_wn` and `_merge` by suffix, alphabetic order or activity.
Missing/malformed version metadata can be resolved by an explicit creator reply
that supplies the actual version and target tuple; retain that reply as the
correction source instead of fabricating source metadata.

## Human intervention

When evidence cannot establish a target, post one actionable branch-confirmation
comment mentioning the creator. Include the issue/bug, version, repository,
all candidates, and any conflict with project context. Ask for the exact target
branch (and missing version/repository if needed). One candidate may be proposed
for explicit confirmation; silence, a reaction, or another user's reply does not
confirm it. A creator can explicitly select a nonmatching branch, but the reply
must explain its association with this bug version; verify its existence.

```bash
multica issue metadata set <issue-id> --key waiting_on --value version_branch_confirmation --type string
multica issue metadata set <issue-id> --key blocked_reason --value "<repository, version, candidates or discovery error>" --type string
multica issue status <issue-id> blocked --no-start
multica issue comment add <issue-id> --content-file <question-file>
```

Use the runtime's `--parent` when replying. Leave the agent assigned and end the
run; a member reply can route back through Multica's comment dispatch. Read the
actual reply actor, not a name quoted in its text. If the required creator is no
longer a workspace member, leave a human handoff and do not invent a replacement.

The comment/reply is the decision record. `waiting_on` and `blocked_reason` are
optional search cursors; losing them on Syndra upsert must neither erase an
existing confirmation nor authorize a missing one. Do not repeat an unanswered
question unless its candidate set or evidence changed.

After a valid decision, fetch `refs/heads/<branch>` explicitly; verify it is an
actual remote branch, not a local ref, tag or SHA. Record `FETCH_HEAD` and the
exact tuple. Clear stale waiting cursors and move to `in_progress --no-start`.

## Checkout and continuation

For `github_repo`, use a managed checkout for discovery if no existing checkout
is available:

```bash
multica repo checkout <repo-url>
```

Before selecting the base, inspect status, branch/history and linked PRs. A
clean worktree may still contain committed work from an earlier run. Only for
a fresh discovery checkout with no work to preserve may you call:

```bash
multica repo checkout <repo-url> --ref refs/remotes/origin/<confirmed-version-branch>
```

Check the returned path, remote URL and `HEAD` against a fresh exact remote
fetch. Multica can continue from cached state after fetch failure; a successful
checkout response alone does not prove a current base. The full remote-tracking ref avoids a same-named tag or local branch. Verify
the actual SHA and align the fresh task branch to the fetched remote commit
if necessary.
Never write directly on the version branch.

Repeated `repo checkout` can reset/clean a checkout or choose a new task branch.
On continuation, reuse the verified existing branch/PR and integrate the new
base with ordinary git operations after preserving work. For a new run without
the old checkout, recover the pushed PR head into the new isolated checkout.
Do not overwrite unpublished previous work; report recovery needs if unavailable.

For a native `local_directory` worktree, the runtime already created a separate
checkout and conversation branch. Do not call `repo checkout` to recreate it.
Check for the runtime's pending merge and prior work. Resolve a pending merge
semantically first. If this is fresh work based on the user's unrelated HEAD,
record that fact and realign only the isolated task branch to the confirmed
remote version base, preserving any user changes for a separate human decision.
Never include unrelated user changes in the bug PR.
