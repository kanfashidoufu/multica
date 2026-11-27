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
- **waiting**: verify `wakeup_id`. When `reply_on_bug=true`, post the marker on its own line in the original Bug, list actual candidates and mention the current human. They can comment **默认 [仓库] 分支名** here or DM the expert the returned `feishu_command` with their branch. Include the repository when several are waiting. An agent's paraphrase is never confirmation. Do not send the user to the internal anchor or ask them to mark anything done. Without this capability use the returned confirmation issue as before. Use `blocked --no-start` before implementation; for a finished patch awaiting only the owner, use `in_review --no-start` and link the diff.
- **Only this Bug**: the member can reply **仅本单 <branch>** directly to the agent's marked comment on the original Bug. This takes precedence for that Bug and repository only. It does not complete the shared confirmation or release any other waiters. Previously confirmed, unambiguously scoped individual replies may still be used for that Bug alone; never promote old replies to a shared default.
- **Stale target**: only a successful lookup proving the ref absent permits `--missing-decision <decision_comment_id>`. On `reopened=true` ask the member for **更换默认 [仓库] 分支名** on the original Bug when inline replies are enabled; otherwise use the confirmation issue. A missing per-Bug override needs a new per-Bug reply. Authentication/network errors are blockers, not evidence that a branch was deleted.
- **Changed assignee/repository/version**: on a wake, before editing, and immediately before either integration, read the current source and run the resolver again. Compare the full scope and decision to the recorded one. Never reuse another member's choice, even for this same Bug. If work/PR already exists, preserve it and report both targets for human handoff; do not automatically retarget. A run acting for a previous assignee, missing/ambiguous member, missing version ID or environment, unavailable resolver, or failed wakeup registration is a blocker; never guess a shared scope or promise a wakeup without a saved rule.

Keep the source human as the branch decision and review contact. Remote branches with `_wn` or `_merge` are legitimate; do not rank by suffix or recent activity. Once the branch is established and fetched, clear stale waiting metadata. Use `in_progress --no-start` when resuming development or integration; keep `in_review` if only acceptance remains. Each Bug still starts from the fresh version tip, not the prior Bug's commit or task branch.

## Checkout and continuation

For a fresh checkout, run the bundled helper from the isolated task directory:

```bash
bash <this-skill-dir>/scripts/ssh-worktree.sh "<exact-workspace-registry-url>" "<bug-key>-<repo>"
```

Use the reported worktree, not its backing clone. This helper checks the registry,
converts a standard HTTPS URL to SSH, preserves explicit SSH ports and never
overwrites an existing directory. Do not use generic `multica repo checkout` for
these fixes: the daemon can substitute the registered HTTPS URL. If a custom
SSH endpoint, host key or credential is missing, ask for it; do not disable host
verification or configure a global URL rewrite.

On the first run, after confirmation, fetch the exact remote branch over SSH,
then create a new Bug-specific working branch at the fetched commit. Never work
on the version branch directly. On resume, inspect status, HEAD, existing PRs
and effective fetch/push URLs before any write; preserve commits and dirty
files. Do not reset an existing worktree. If an older checkout has an HTTPS
origin, preserve its work and use the equivalent verified SSH URL explicitly
for Git transfers; do not rewrite a shared origin. Use the SSH-only worktree
for integration and the bundled verifiers. Missing SSH access is a blocker.
