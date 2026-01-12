# Deno Log

### 2026.02.11

Rebased Deno changes onto latest `main` (be25a2b57).

- `git checkout -b rebase/2026-02-11 main`
- `git merge --squash origin/rebase/2026-01-12`
  - All 7 commits from `rebase/2026-01-12` squashed into one.
  - Resolved 11 conflicted files. Main had moved 127 commits ahead.
- Notable conflict resolutions:
  - `internal/api/`: main restructured the API with StdioServer/Transport/Protocol/
    Session/Connection architecture. Kept the Deno fork's Server with MessagePack
    protocol and callback system instead. Removed main's new architecture files
    (callbackfs, conn, protocol, session, transport).
  - `internal/checker/checker.go`: merged main's new `moduleSymbols` field with
    Deno's `denoGlobalThisSymbol`/`nodeGlobalThisSymbol`/`denoForkContext`.
  - `internal/ls/autoimport/extract.go`: merged rebase's `SymbolTable` interface
    methods (`.Len()`, `.Iter()`) with main's `ExportStar` filtering fix.
  - `internal/project/snapshotfs.go`: merged main's `convertOpenAndCloseToChanges`
    with rebase's exported `SourceFS` type.
  - `internal/project/session.go`: merged main's `UserConfig` fields with rebase's
    `makeHost`.
  - `internal/lsp/server.go`: removed `handleInitializeAPISession` (depends on
    deleted API architecture).
- Additional build fixes:
  - Updated `SymbolTable` map access to interface methods in `binder.go`, `checker.go`.
  - Updated `GetAmbientModules` callers to pass `nil` sourceFile parameter.
  - Updated `api.go` to match new `OpenProject` (4 return values) and
    `NewLanguageService` (now takes `ls.Host` + `activeFile`) signatures.
  - Added `GetAccessibleEntries` to `builderFileSource` to satisfy `FileSource`
    interface.

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
