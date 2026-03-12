# Deno Log

- Merged upstream commits: https://github.com/denoland/typescript-go/commits/4a59cd78390d5789f547db8af35b43be2f829719
- Divergences in this fork: https://github.com/denoland/typescript-go/compare/4a59cd78390d5789f547db8af35b43be2f829719...main
- Keep this updated with the hash of the latest merged upstream commit.

### 2026.03.09 / @nayeemrmn

We're switching to regular 3-way merges with merge commits.

- `git checkout main`
- We're merging up to https://github.com/denoland/typescript-go/commits/4a59cd78390d5789f547db8af35b43be2f829719.
- The target commit is 4a59cd78390d5789f547db8af35b43be2f829719.
- The date we'll reference for this merge is that of the target commit, not
  necessarily the current date.
- `git merge -m "merge(2026-03-09): 4a59cd78390d5789f547db8af35b43be2f829719" 4a59cd78390d5789f547db8af35b43be2f829719`
    - General message format: `merge(YYYY-MM-DD): <hash-of-merged-commit>`
  - Resolve conflicts.
  - Update this log file in the process.
  - `git add .`
  - `git merge --continue`.

### 2026.01.12 / @bartlomieju

Changes to the rebase process to avoid squashing all our changes into a single commit.

- `git checkout rebase/2026-01-12`
  - We're at `6e1e2c29067d9dfe638301be2d6409e788df47b1` from `Microsoft/typescript-go`
- `git pull <denoland-remote> rebase/2025-12-16`
- `git checkout 6e1e2c29067d9dfe638301be2d6409e788df47b1`
- `git checkout -b rebase/YYYY-MM-DD`
  - We're now up to date with `Microsoft/typescript-go` main branch.
- Cherry-pick commits from `rebase/2025-12-16` one by one:
  - `git cherry-pick <commit-hash>`
  - Alternatively all in the same go: `git cherry-pick 07d4196df 158136cda 9b3df7abd d44b7a632 <other-commits>`
  - Resolve conflicts if any.
  - `git add .`
  - `git cherry-pick --continue`
- `git push -u <denoland-remote> rebase/YYYY-MM-DD`
- Change pushed branch to be the default working branch in `denoland/typescript-go` repo.

### 2025.12.19 / @dsherret

- Improved automatic lib.node.d.ts injection by deferring `/// <reference lib="node" />` from being injected.
  - This now waits until after everything has loaded in order to decide whether to inject.

### 2025.12.18 / @dsherret

- Added a `resolveJsxImportSource` method to the resolver for resolving the jsxImportSource based on the referrer.
  - This doesn't support resolving for transforms because we don't use any transform code from TypeScript.

### 2025.12.16 / @nayeemrmn

- `git checkout <main-working-branch>`
  - This time it's `main`, but in the future it may be the most recent
    `rebase/YYYY-MM-DD` branch.
  - We're at commit 383c1a6b97d917bb8454d87827387f886ea0b655.
- `git pull <denoland-remote> <main-working-branch>`
- `git checkout -b main_squashed`
  - The existing changes in the fork will be squashed into one commit on this
    branch.
- `git reset --soft <upstream-commit-last-rebased-on>`
  - This time it's d1be94b3c21175dac7b631f952a737a2995d59da.
- ```
  git commit -m "Deno changes rebased from 383c1a6b97d917bb8454d87827387f886ea0b655

  Co-authored-by: nathanwhit <nathanwhit@users.noreply.github.com>
  Co-authored-by: dsherret <dsherret@users.noreply.github.com>"
  ```
  - The tag mentioned is the current commit from `<main-working-branch>`.
  - These are the authors of the changes being squashed.
- `git diff <main-working-branch>`
  - Verify that this is empty and no changes were lost.
- `git checkout -b rebase/YYYY-MM-DD`
  - Today's date: `rebase/2025-12-16`.
- `git fetch <microsoft-remote>`
- `git rebase <microsoft-remote>/main`
  - Resolve conflicts.
  - While resolving conflicts, write this log file.
  - `git add .`
  - `git rebase --continue`
- `git push -u <denoland-remote> rebase/YYYY-MM-DD`
- `git branch -D main_squashed`
