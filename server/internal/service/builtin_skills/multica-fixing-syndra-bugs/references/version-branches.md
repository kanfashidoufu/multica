# Personal version branch decisions

A default belongs to one **workspace + Syndra source environment + repository + version ID + current human assignee UUID**. The server resolves the unique member from the current synced assignee. Do not use the issue creator, agent ID, suffix preference, or another developer's confirmation as a substitute. An ordinary project ref or a version-wide mapping is only a suggestion.

In the verified checkout list live candidates with the bundled helper:

```bash
bash <this-skill-dir>/scripts/find-version-branches.sh "<bug_version_name>" "<checkout-path>"
multica issue syndra-branch <bug-issue-id> --repo "<credential-free-origin-url>" \
  --candidate "<remote-candidate-1>" --candidate "<remote-candidate-2>"
```

Pass one candidate flag per actual match, or none when there are none. Network failure is not an empty list. A single fuzzy match is still only a proposal. Do not change the workspace/project repo settings.

- **issue_override / personal_default**: fetch the exact `refs/heads/<branch>` from the verified origin and record its commit. In the Bug thread record the response's marker verbatim on its own line, repo, version, member, branch and decision comment ID. Use the existing per-Bug worktree. A subsequent Bug with the same personal default proceeds without asking.
- **waiting**: the command finds or creates one shared confirmation issue assigned to the member. For an assigned agent run it also registers a one-time native wakeup for this Bug when the confirmation issue reaches done (check `wakeup_id`). Move the Bug to `blocked --no-start` and leave a link to the confirmation issue plus the marker in the Bug thread. Do not send another branch question; other Bugs in this scope use the same ticket. The member replies **默认 <branch>** on that ticket, or **确认** for its sole proposal; only **更换默认 <branch>** replaces an existing default. The server records their comment and completes the confirmation issue, not the Bug.
- **Only this Bug**: the member can reply **仅本单 <branch>** directly to the agent's marked comment on the original Bug. This takes precedence for that Bug and repository only. It does not complete the shared confirmation or release any other waiters. Previously confirmed, unambiguously scoped individual replies may still be used for that Bug alone; never promote old replies to a shared default.
- **Stale target**: a successful remote lookup proving that the personal branch is gone permits repeating the command with `--missing-decision <decision_comment_id>`. Only the caller receiving `reopened=true` posts a replacement request on the shared ticket, mentioning the member and listing fresh candidates. A missing per-Bug override needs a new per-Bug reply. Never invalidate a default because of an authentication or network failure.
- **Changed assignee/repository/version**: on a wake, before editing, and immediately before either integration, read the current source and run the resolver again. Compare the full scope and decision to the recorded one. Never reuse another member's choice, even for this same Bug. If work/PR already exists, preserve it and report both targets for human handoff; do not automatically retarget. A different agent owner, missing/ambiguous member, missing version ID or environment, unavailable resolver, or failed wakeup registration is a blocker; never guess a shared scope or promise a wakeup without a saved rule.

Keep the source human as the branch decision and review contact. Remote branches with `_wn` or `_merge` are legitimate; do not rank by suffix or recent activity. When a branch has been established and fetched, clear stale waiting metadata and move the Bug to `in_progress --no-start`. Each Bug still starts from the fresh version tip, not the prior Bug's commit or task branch.

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
