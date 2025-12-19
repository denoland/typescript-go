# Deno Log

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
