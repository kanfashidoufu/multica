---
name: multica-fixing-syndra-bugs
description: "Handle Feishu DMs to the Bug expert about choosing a repair branch, and deliver enrolled Syndra fixes to the verified version/test branches."
user-invocable: false
allowed-tools: Bash(multica *), Bash(git *), Bash(gh *), Bash(bash *)
---

# Deliver Syndra bug fixes to version and test branches

In the Bug expert's Feishu DM, interpret branch choices conversationally using [references/feishu-choices.md](references/feishu-choices.md); do not repair code from the chat run.
On the assigned Bug run, verify local validation, CI, remote version/test integration and review notification. A PR alone is incomplete. The current Syndra assignee makes branch decisions.

Read `multica-platform/references/issues.md`, `projects.md`, and `mentions.md`
when using their status, checkout, PR and reply contracts. These live under the
sibling `multica-platform` skill's `references/` directory.

## 1. Recover scope and prior work

Read the current issue and its relevant comment threads using the runtime's
commands. Confirm `external_source=syndra`, `multica_bug_automation=true`, and
`creator_type=member`. Use the branch resolver's current member UUID, verified
against the workspace roster. The importer controls pilot enrollment; the
shared workspace agent `bug修复专家` executes the repair. Use the current Bug
assignee's identity for decisions, including on resumed runs.

Use the actual `bug_id`, `bug_version_id`, `bug_version_name`, environment,
reproduction steps and source evidence. An environment branch (`dev`, `test`,
`pre`, or deployed production HEAD) is not the release destination.

Before any checkout/reset, inspect existing files, `git status`, the current
branch, prior comments and `multica issue pull-requests <issue-id> --output json`.
Resume an existing fix/PR rather than creating a duplicate. A missing metadata
cursor is not a missing decision: Syndra sync replaces mirror metadata; comments
and the provider's current PR state carry the recoverable evidence. Page back
through relevant threads when a decision is older than the latest 30 replies.

Read the workspace repo registry (URLs and descriptions) with `multica repo list --output json`
or the `repos` field of `multica workspace get --output json`. Shortlist by module,
error, route or API, then verify the responsible code in SSH-only isolated worktrees.
If evidence cannot identify the repo, ask the assignee. For a cross-repository fix, keep a separate
branch decision, isolated checkout, PR and delivery proof per repository. The
issue is not delivered until all affected repositories are integrated.

## 2. Confirm the exact version branch

Read [references/version-branches.md](references/version-branches.md) for the
selection rules, human confirmation and safe checkout procedure. Finish that
procedure before editing product code. Record the repository URL, bug/version
identity, exact remote branch, evidence source, and fetched base SHA in a comment.

A valid personal decision selects the exact remote branch even if many other
branches match. Without a valid decision, fuzzy matches are proposals only.
Stop for conflicting evidence or an unverifiable target; never substitute an
environment, default or nearby branch.

On resume and immediately before merge, re-read the issue/version and the branch
decision. A change to the repository, bug version, or decision invalidates the
previous target. Preserve work, explain the mismatch and request confirmation;
do not silently retarget an existing PR or discard previous commits.

## 3. Diagnose, fix and validate

Follow the target repository's instructions. Reproduce or establish concrete
evidence, identify the root cause, and record a concise plan: affected files,
minimum behavior change and regression checks. Then implement in the same run;
there is no routine plan approval. Missing evidence that prevents a defensible
fix is a blocker. Use relevant QTB diagnostic tools/skills if they are installed
and applicable; do not assume private tools exist on every runtime.

Follow the SSH checkout procedure; set `GIT_ALLOW_PROTOCOL=ssh` for all Git transfers,
including submodules. Never fall back to HTTPS or change shared Git configuration.
Reuse an isolated worktree only with verified SSH fetch and push endpoints. Preserve
prior-turn changes and keep one PR per Bug per repo. Never edit an in-place
user checkout or share working files between concurrent bugs.

Add a meaningful regression test where practical, make the minimum fix and run
the repository's relevant checks. Inspect the final diff for unrelated work and
secrets. If the root cause or affected repositories change, update the plan and
branch decisions before expanding the patch.

For enrolled Bug fixes, use ordinary Git commits; skip the QTB local commit
gate (`qtb-local-commit-gate`) and its receipt. Validation, CI and repository
branch protection still apply.

Fetch the exact confirmed branch before final testing. Incorporate its changes
on the task branch, resolve conflicts semantically and rerun affected checks.
Never blanket-accept ours/theirs or overwrite another bug's work. If rebasing a
published task branch is necessary, use `--force-with-lease` and investigate an
unexpected remote head before proceeding. Never force-push a version branch.

## 4. Integrate and prove delivery

Read [references/version-delivery.md](references/version-delivery.md). Open or
update each PR with its confirmed version branch as base. Include the issue key
in the title to link it in Multica, but omit `Fixes/Closes/Resolves <issue-key>`:
those keywords can let one PR close the issue before delivery proofs; `done`
stays with human acceptance.

After local validation and required CI pass, merge the validated fix into the
confirmed version branch, push it, then merge that version branch into remote
`test` and push it. Resolve conflicts semantically in the isolated integration
checkout; after a successful resolution notify the current human assignee.
If the fix is ready and only the assignee's confirmation or PR approval remains,
set `multica issue status <issue-id> in_review --no-start` now. Mention the
assignee with the diff/PR, checks, exact decision needed and outstanding merges.
When confirmed, resume any remaining integration; do not add a routine approval.

If validation or required CI fails, commit the current fix snapshot, merge that
snapshot into the confirmed version branch and push it, then set the issue to
`blocked --no-start`. Do not promote the version branch to `test`. Comment with
the failed checks, version branch commit and the exact action needed from the
current human assignee; preserve the task branch and checkout for continuation.

For GitHub, run the bundled verifier after the provider reports a completed
merge, using the recorded validated PR head:

```bash
bash <this-skill-dir>/scripts/verify-version-delivery.sh \
  <checkout-path> <confirmed-version-branch> <canonical-pr-url> <validated-head-sha>
```

Only `delivery=merged` with exit status 0 proves the version branch delivery.
Then run the test-branch verification described in the reference. Both remote
facts are required before reporting delivery. Squash/rebase merges must use the resulting
merge commit, not assume the original PR head remains an ancestor. Failed
fetches, shallow history or a missing merge commit are unverified, not success.

## 5. Report the actual outcome

After every affected repo has proof, post one result in the appropriate comment
thread with `--content-file` and the runtime-provided `--parent` when applicable.
Start its first paragraph with `处理结果：` and summarize the fix and actual verification
for Syndra. Only that paragraph is copied when the issue is marked done. Put internal
handoff details after a blank line; never include credentials in the summary.
Mention the verified current human as `[@Name](mention://member/<scope.member>)`. Include:

- bug root cause and minimal change;
- each repository, confirmed version branch and decision evidence;
- PR URL, validated head, version merge commit and fetched version tip;
- test merge commit and fetched test tip;
- tests and CI results, remaining risks and optional verification steps.

After both integrations, set or keep `in_review` for final human acceptance.
Report the merged result to the current assignee. For local verification, offer
a detached worktree at the test merge commit and the repository's setup commands.

If blocked, keep the issue `blocked`, clearly state that version integration is
incomplete, and identify the exact missing decision/check/access or conflicting
PR. Preserve the worktree and pushed branch. Post enough context for a later
human reply/retry to resume without repeating work. If no code change is needed,
report that finding for human decision without marking the automation delivered.
