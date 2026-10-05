---
name: code-quality-check
description: Use when the user asks for a code quality check, quality audit or health check of the devdash codebase, or runs /code-quality-check. Not for reviewing one pull request (that is the pr-reviewer agent) and not for bug or security hunting.
---

# Code quality check

Two independent read-only audits, one for the interface code and one for the core, then one short summary. Nothing is fixed, committed or ticketed by this skill.

## Steps

1. Spawn two agents in a single message so they run in parallel. Both: `subagent_type: "general-purpose"`, `model: "opus"` (Opus 5.5). Do not audit inline and do not merge the two into one agent.
   - Interface agent: scope `cmd/devdash/` and `internal/tui/`.
   - Core agent: scope `internal/model/`, `internal/collector/`, `internal/engine/`, `internal/docker/` and `internal/freeport/`.
   - Linter line, the same for both: golangci-lint v2.13.2 (pinned in CI), config `.golangci.yml` (`default: standard` plus `gocognit`, `misspell`, `modernize`, `unconvert` and `unparam`; gocognit `min-complexity: 60`, a ratchet just above the worst non-test function, with `_test.go` files excluded; formatters `gofmt` and `goimports`), run by `make lint` for darwin and linux.
2. Give each agent the brief below with its scope and the linter line filled in. If the user passed an argument (a path or a topic), add it to both briefs as a narrower scope.
3. Wait for both reports. Do not start the summary with one report missing.
4. Write the summary in the format below.

## Agent brief

```
Audit <scope> of this repository for code quality. Read-only: do not edit, commit, or write to Jira or GitHub.

Read first: CLAUDE.md, the part of docs/SPEC.md (the authority) that covers the area, and DECISIONS.md before you judge a design choice. A choice recorded in DECISIONS.md, required by the spec, or marked with a `ponytail:` comment is not a finding. CLAUDE.md fixes these, so they are not findings either: no cgo, no shelling out to lsof/ss/netstat/ps on the refresh path, the short dependency list, stdlib `flag`, no Bubbles.

Look for exactly these four things:
1. Simplification: code that a shorter construct, the standard library, an installed dependency or an existing helper in this repo replaces; duplicated logic; dead code. `_test.go` files are in scope (shared fixtures exist, such as internal/tui/helpers_test.go); golden files and testdata/ are not. Per-OS sibling files (`*_darwin.go` / `*_linux.go`) mirror each other on purpose: they are a duplication finding only when the shared part has no OS-specific call in it.
2. Performance: only with a concrete mechanism and a realistic trigger. Examples: work repeated on every refresh tick or per process (a syscall or file read per process that one call could batch, O(n²) over processes or listeners, parsing the same data twice in one refresh), work repeated on every key press or View() call in the TUI. No micro-optimizations. `make bench` runs the benchmarks if a number supports the claim.
3. Code smell: long functions, deep nesting, boolean-flag parameters, primitive obsession, unclear names, inconsistent handling of the same case in sibling code.
4. Misused abstraction: an interface with one implementation, a wrapper that adds nothing, a state type or message split that hides a simple flow, a layer that leaks (OS-specific code outside build-tagged files, display text that both the dashboard and `port N` show, built somewhere other than internal/model/format.go, terminal or TUI concerns inside internal/model), a pattern applied where a function would do. The `Collector` interface, with one implementation per OS plus the test `Fake`, is by design.

Not in scope: bugs, security, formatting, missing tests.

Start from the largest and most-changed files, then follow what you find. Confirm every finding in the code (read the callers) before you report it. Report the ten findings with the highest payoff at most, ranked. Fewer is fine.

For each finding, name a linter change that would catch this class of mistake. Current setup: <linter line>. Prefer a linter or rule that golangci-lint already bundles: enabling it in .golangci.yml is not a new dependency. Give the linter name and its exact settings keys. Next, a tool outside golangci-lint (say that it is a new dependency). Check that the rule exists (`golangci-lint linters` lists the bundled linters). If no rule catches it, write "none".

Report format, one block per finding, nothing else:
<path>:<line> | <simplification|performance|smell|abstraction>
Problem: one sentence.
Fix: one sentence.
Lint: <linter and settings in .golangci.yml, or new tool and rule> or none
```

## Summary format

```
## Interface (<n> findings)
1. `path:line` category: problem. Fix: fix.
...

## Core (<n> findings)
1. ...

## Linter changes
- .golangci.yml: <linter and settings>. Catches: interface 1, 4.
- New dependency: <tool>, <rule>. Catches: core 2.
```

- Keep each agent's ranking. One line per finding.
- Merge linter suggestions that name the same rule, and list which findings each one catches. Leave out findings with `Lint: none` from that section.
- Drop a finding that has no `path:line`. Add no findings of your own.
- No praise, no preamble, no closing advice. A side with no findings gets the line "No findings."
