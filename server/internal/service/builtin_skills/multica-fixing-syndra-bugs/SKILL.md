---
name: multica-fixing-syndra-bugs
description: "Use for code-changing Syndra bugs enrolled in the 王宁 pilot (external_source=syndra, multica_bug_automation=true), including version-branch confirmation, resumed fixes, validation-gated version/test integration and review handoff."
user-invocable: false
allowed-tools: Bash(multica *), Bash(git *), Bash(gh *), Bash(bash *)
---

# Deliver Syndra bug fixes to version and test branches

The acceptance criteria are validation, CI, merge into the confirmed version
branch, promotion to remote `test`, and review notification. A PR or auto-merge
setting alone is incomplete. Keep the original member creator as branch
decision and review contact; respect Agent Identity limits.

Read `multica-platform/references/issues.md`, `projects.md`, and `mentions.md`
when using their status, checkout, PR and reply contracts. These live under the
sibling `multica-platform` skill's `references/` directory.

## 1. Recover scope and prior work

Read the current issue and its relevant comment threads using the runtime's
commands. Confirm `external_source=syndra`, `multica_bug_automation=true`,
`bug_assignee_name=王宁`, and `creator_type=member`. Resolve `creator_id` against
`member.user_id` from `multica workspace member list --output json`; require that
this is the uniquely named 王宁 member, not a membership row ID. Other members'
bugs are outside the pilot, even when their agent has this built-in skill.
If assignment changed away from 王宁, stop automation and explain the handoff.

Use the actual `bug_id`, `bug_version_id`, `bug_version_name`, environment,
reproduction steps and source evidence. An environment branch (`dev`, `test`,
`pre`, or deployed production HEAD) is not the release destination.

Before any checkout/reset, inspect existing files, `git status`, the current
branch, prior comments and `multica issue pull-requests <issue-id> --output json`.
Resume an existing fix/PR rather than creating a duplicate. A missing metadata
cursor is not a missing decision: Syndra sync replaces mirror metadata; comments
and the provider's current PR state carry the recoverable evidence. Page back
through relevant threads when a decision is older than the latest 30 replies.

Identify every affected repository from project resources, workspace repos,
code and reproduction evidence. Do not pick a repo merely because it is the
first or only cached checkout. For a cross-repository fix, keep a separate
branch decision, isolated checkout, PR and delivery proof per repository. The
issue is not delivered until all affected repositories are integrated.

## 2. Confirm the exact version branch

Read [references/version-branches.md](references/version-branches.md) for the
selection rules, human confirmation and safe checkout procedure. Finish that
procedure before editing product code. Record the repository URL, bug/version
identity, exact remote branch, evidence source, and fetched base SHA in a comment.

A single fuzzy match is only a candidate. Zero/multiple matches, inconsistent
version evidence, missing branches, or unavailable remote state require human
intervention. Never substitute the default branch, a test environment branch,
or a nearby version to keep the run moving.

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

Use one isolated task branch and one PR per repository for this bug. Never edit
the shared `local_directory` in `in_place` mode. Multica now supports
`local_directory.execution_mode=worktree`: use the returned managed worktree,
including its conversation branch and prior-turn work. Verify its identity and
base first. Do not switch the user-owned source directory or recreate a managed
checkout after edits. For `github_repo`, use `multica repo checkout` as described
in the branch reference. A native local worktree can be used without a
`github_repo` resource if its verified remote and forge support the delivery
procedure.

Add a meaningful regression test where practical, make the minimum fix and run
the repository's relevant checks. Inspect the final diff for unrelated work and
secrets. If the root cause or affected repositories change, update the plan and
branch decisions before expanding the patch.

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
Required reviews/checks and uncertain conflicts remain technical blockers.

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
facts are required before review. Squash/rebase merges must use the resulting
merge commit, not assume the original PR head remains an ancestor. Failed
fetches, shallow history or a missing merge commit are unverified, not success.

## 5. Report the actual outcome

After every affected repo has proof, post one result in the appropriate comment
thread with `--content-file` and the runtime-provided `--parent` when applicable.
Mention the verified creator as `[@王宁](mention://member/<creator_id>)`. Include:

- bug root cause and minimal change;
- each repository, confirmed version branch and decision evidence;
- PR URL, validated head, version merge commit and fetched version tip;
- test merge commit and fetched test tip;
- tests and CI results, remaining risks and optional verification steps.

Set `in_review` only after both remote integrations are proven, using
`--no-start` when changing status for work already in progress. Notify the
current human assignee and ask them to review the merged result; do not claim a
production deployment occurred. Use the target repository's own setup commands
if offering a detached review worktree at the test merge commit.

If blocked, keep the issue `blocked`, clearly state that version integration is
incomplete, and identify the exact missing decision/check/access or conflicting
PR. Preserve the worktree and pushed branch. Post enough context for a later
human reply/retry to resume without repeating work. If no code change is needed,
report that finding for human decision without marking the automation delivered.
