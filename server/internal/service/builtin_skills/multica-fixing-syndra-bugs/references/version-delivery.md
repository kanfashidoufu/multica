# Integration and delivery proof

## PR and validation

Read `multica issue pull-requests <issue-id> --output json` to recover links and
avoid duplicate PRs. `state` is an enum (`open`, `draft`, `closed`, `merged`), not
a collection of booleans. `snapshot_available=false` or `snapshot_stale=true`
means missing/stale CI evidence. Null checks are not green. Fetch authoritative
provider state when the link/snapshot is unavailable or stale; document a
missing Multica link instead of assuming the PR does not exist.

For GitHub, verify the repository and PR base/head, and link the issue through
the title. Always pass an explicit PR and repository context; do not rely on a
user's current `gh` repository selection.

```bash
gh pr create --base <confirmed-version-branch> --head <task-branch> \
  --title "<issue-key>: <fix summary>" --body-file <pr-body-file>
gh pr view <pr-url> --json url,state,baseRefName,headRefName,headRefOid,mergeStateStatus,reviewDecision,statusCheckRollup
```

Run these commands from the verified checkout. Record repository/version/branch,
branch-decision evidence, root cause, minimal patch, validation results and all
other required PRs in the body. Do not include a closing keyword with the issue
key: one webhook must not prematurely close a multi-repo fix.

Before merge, fetch the current version branch and inspect other open PRs to
that base for file overlap. Integrate required base changes, resolve conflicts
semantically, rerun relevant local tests and CI, and record the resulting PR head.
Overlapping concurrent changes need the repository's merge queue or strict
up-to-date protection; otherwise stop with the overlap and ask for integration
coordination. Never use an administrator bypass.

The imported issue's acceptance criteria explicitly require the CI result. Under
the runtime's foreground-wait exception, collect it in one foreground call:

```bash
gh pr checks <pr-url> --watch
```

Respect the runner's timeout; do not start background watchers, sleep/poll loops,
or assume a later completion wakes this run. If required CI is unavailable,
failing or still pending when the call ends, report `blocked` with the PR and
required next action. A repository that defines no CI must have that fact
verified from its configuration/protection rules; a missing snapshot is not
such evidence. Local checks remain required.

## Merge and verify

Re-read the creator's branch decision and the current issue version. Verify that
this PR's repository/base still matches, its current head equals the tested
head, required checks/reviews passed, and merging is allowed. Then merge with
the repository's allowed strategy, binding the request to that head:

```bash
gh pr merge <pr-url> --match-head-commit <validated-head-sha> <allowed-strategy>
```

Choose `--merge`, `--squash` or `--rebase` according to repository policy. For a
merge queue, follow its required flow. Command success can mean only auto-merge
was enabled or queue entry was created. Query the PR again; it must actually be
`MERGED`, with a merge commit and the confirmed base. If it is still queued,
leave integration explicitly incomplete and request a later retry/reply; do not
claim a background wakeup or create recurring automation without authorization.

Run `scripts/verify-version-delivery.sh` with the canonical PR URL and the exact
validated head. The verifier fetches the confirmed remote branch and checks the
provider's merge commit is an ancestor of the fetched tip. If history is shallow,
fetch sufficient target history and rerun the verifier. If the branch was deleted,
rewritten or the merge is absent, stop and investigate. Do not replace a failed
check with issue status, PR prose or a local branch tip.

For another forge supported by the project, use its available authenticated
CLI/API and perform the same checks: exact repo/base, validated head, merged
state and merge commit contained in a fresh remote target fetch. The bundled
GitHub verifier does not support other providers. If these facts cannot be
verified, require human integration help instead of reporting success.

On resumed work where the PR has already merged, recover the validated head and
branch decision, then verify remote containment before making any edits. If
historical validation evidence is missing, inspect the delivered commit and run
appropriate verification; never invent a prior successful check.

## Review handoff

Only after every required repository has delivery proof, set `in_review` and
report the tuple `(repo, version branch, PR, validated head, merge commit,
fetched version tip)` with tests. `done` is human acceptance. A source-system
"resolved" status or a completed agent run is not proof of version integration.

If a PR was merged to the wrong branch, preserve that fact, request any missing
branch confirmation, and deliver a corrective PR to the confirmed target.
Never call a wrong-base merge complete or silently rewrite the version branch.
