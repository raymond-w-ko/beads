# Upstream

This fork tracks `gastownhall/beads` (remote `upstream`). Fork branches go to `raymond-w-ko/beads` (remote `origin`). This file records what the fork posted upstream and what it keeps. Updated 2026-10-17 at fork main 9d6725a8d, upstream/main a3e1b24e2.

## Posted

| Kind | Number | Title | Fork commits | Branch | State (2026-10-17) |
|------|--------|-------|--------------|--------|--------------------|
| PR | [#7570](https://github.com/gastownhall/beads/pull/7570) | test: isolate Dolt credentials and agent settings in the test env | cab3d5cb3 (test-env.sh part) | pr/test-env-unset-agent-vars | open, approved by a collaborator, waits for a maintainer Bazel run |
| PR | [#7571](https://github.com/gastownhall/beads/pull/7571) | fix(dolt): roll back write transactions on panic | de6dd59f7 (Rollback part) | pr/commit-write-tx-panic-rollback | open, approved by a collaborator, waits for a maintainer Bazel run |
| PR | [#7575](https://github.com/gastownhall/beads/pull/7575) | test(scripts): route test hooks to the prebuilt bd | ac1f166a1, a8716a50d (coreutils PATH part) | pr/test-hooks-prebuilt-bd | open, approved by a collaborator, waits for a maintainer Bazel run |
| Issue comment | [#6117](https://github.com/gastownhall/beads/issues/6117#issuecomment-6200787940) | write transactions span the whole command (Dolt 1213) | 8b5f85b1a, de6dd59f7, 9be18c2b9 (described only) | - | issue open, no reply |

Beads: bd-ksvg (catalogue), bd-l2jn (upstream matches), bd-so6v (operator selection), bd-8irlo (this status).

## Not posted yet

| Fork commits | What | Plan |
|--------------|------|------|
| 8b5f85b1a, de6dd59f7 (NoJournal guard), 9be18c2b9 | journal writer lock (GET_LOCK on bd_events_seq) | PR rebased on upstream #7244; draft bd-7ucf, held for operator confirmation |
| 5d08ebcce (fork addition only) | defer-wake sweep must not run during a migration freeze | small PR on top of upstream #6424 |
| 9d6725a8d | `relation` field on bd show --json dependency rows | PR (feature) |
| ac1f166a1 (Bazel half) | test-bd launcher in tools/bazel/test_env.sh | addition to #7575 or a follow-up PR |
| da73d6e70 / dependency_cycle_chain_test.go | deep-chain cycle test | optional test-only PR, not approved |

## Already upstream

| Fork commits | Upstream |
|--------------|----------|
| 7ac4628bb, da73d6e70 | ba12ffa74 (#7224) |
| 5d08ebcce (except the freeze check) | 9d646fc88 (#6424) |
| merge 1a26e3afe and its 35 release commits | tag v1.3.1 |

## Fork-only

| Fork commits | Why |
|--------------|-----|
| e6d342069, ecce4664a, 7dfdc78f4, 51902b39b, b7aed9203, 784f3cdb5, 38598302d | remote_dolt_only build: no local Dolt, external server on 3307, dolt-db and beads/beads defaults, remote-only `make build`, test skips |
| a8716a50d (credentials_test.go part) | needed only for the remote_dolt_only default host |
| c9ba431a6 | test Dolt server for the remote-only build |
| cab3d5cb3 (AGENTS.md part) | everturn checks |
| e017d0679 | fork glossary |

## How to refresh

```sh
git fetch upstream
git log --oneline upstream/main..main                      # every fork-side commit
git log --no-merges --format='%h %an %s' upstream/main..main
git cherry -v upstream/main main | grep '^-'               # patch-equal on upstream/main
git tag --contains <commit>                                # release-line commits (v1.3.1)
git log --oneline upstream/main --grep '<#PR or subject>'  # reached upstream?
git log --oneline upstream/main -S '<identifier>'
git merge-tree --write-tree --name-only --merge-base <c>^ upstream/main <c>  # does <c> still apply?

gh pr list --repo gastownhall/beads --author raymond-w-ko --state all --json number,title,state,url
gh search issues --repo gastownhall/beads --commenter raymond-w-ko --json number,title,state,url
gh pr view <n> --repo gastownhall/beads --json state,reviewDecision,mergeStateStatus,statusCheckRollup,updatedAt,comments,reviews
gh issue view 6117 --repo gastownhall/beads --json state,updatedAt,comments
```
