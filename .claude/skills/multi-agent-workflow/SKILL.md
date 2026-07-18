---
name: multi-agent-workflow
description: How the main thread orchestrates the architect → developer → tester → security → techwriter loop in this repo. Load when about to start any non-trivial change so the right agent is invoked at the right time with the right context.
---

# Multi-agent workflow

The `.claude/agents/` directory defines five roles. They run as **subagents**: the main thread invokes them via the `Task` tool; the architect can re-invoke other agents directly when the harness allows it.

## The five roles

| Agent | Model | Owns | Never does |
|---|---|---|---|
| **architect** | opus | analysis, decisions, delegation, final review | writes code |
| **developer** | sonnet | code, why-comments, unit tests, self-verification | designs / decides scope |
| **tester** | sonnet | running tests, coverage gap analysis | implements features |
| **security** | opus | CVE / attack-surface / interaction review | writes code, runs unrelated tests |
| **techwriter** | sonnet | comment audit, `just docs` regen, README/CLAUDE.md updates | invents undocumented features |

## When to invoke

| Situation | Start with |
|---|---|
| User wants a new feature / behavior change / refactor / non-trivial bugfix | **architect** |
| User wants a code-only task with crystal-clear instructions ("rename X to Y in file Z") | **developer** directly |
| User wants "are the tests still green?" / coverage report | **tester** directly |
| User wants a security review of branch | **security** directly |
| User wants docs / comments cleaned up only | **techwriter** directly |
| User says "ultrareview" | **explain** - it is the user-triggered `/code-review ultra` command (`/ultrareview` is its deprecated alias); agents cannot launch it |

For everything ambiguous or with > 1 file in scope, start with **architect**. The architect's job is to refuse fuzzy work and to keep the design honest.

## Standard loop (architect-led)

```
main → architect
         ↓ analyze & accept/reject
         (reject → back to main with reason)
         ↓ delegate
       developer
         ↓ implement + self-verify
       architect (review)
         ↓ delegate (or loop back to developer)
       tester
         ↓ run + coverage gap report
       architect (review)
         ↓ delegate (or loop back to developer)
       security
         ↓ findings report
       architect (review)
         ↓ delegate (or loop back to developer + tester)
       techwriter
         ↓ comments + docs + CLAUDE.md
       architect
         ↓ final summary
main thread
```

## Two modes — delegation works vs. doesn't

**Mode A: subagents can call subagents.**
The architect has the `Task` tool and invokes developer/tester/security/techwriter directly. Main thread only sees the architect's final summary.

**Mode B: subagents cannot call other subagents.**
The architect produces **exact, self-contained instructions** for each step ("dispatch instruction") and returns them to main thread. Main thread runs the next agent with those instructions and feeds the result back to the architect. The architect remains the brain; the main thread is a relay.

In both modes the architect's job is the same. The harness decides which mode is active.

## What good delegation looks like

Bad:
> "developer, please update the resource to support exec bit"

Good:
> "developer:
> - Edit `internal/provider/files_resource.go` around the `fileModel` struct (currently lines 120–140) to add `ExecuteFilemode types.Bool` with json tag `execute_filemode`.
> - In `diffActions` (around line 380), emit a separate `chmod` CommitActionOptions when `execute_filemode` flipped and content did not change.
> - Preserve action-diffing invariant: zero actions → no commit.
> - Acceptance criteria: existing `TestUnit_diffActions_*` tests pass unchanged; add `TestUnit_diffActions_chmodOnly` covering the flip-without-content-change case.
> - Self-verify: `go test ./internal/provider -run TestUnit_diffActions -v && just check`."

The good version is what the architect produces. If you're tempted to send something vaguer, stop and read the code first.

## How to invoke (main thread, mode B)

```
Task(
    description="Architect review of <task>",
    subagent_type="architect",
    prompt="<full context: user request, relevant file:line citations, acceptance criteria>"
)
```

Then, when the architect returns a dispatch instruction for `developer`, the main thread invokes:

```
Task(
    description="<short task>",
    subagent_type="developer",
    prompt="<exact instruction from architect>"
)
```

And feeds the developer's report back into the architect as a follow-up `Task` call with the prior context plus the new report.

## Stop conditions

- Architect rejects the plan → main reports back to user, no further agents run.
- Developer cannot satisfy acceptance criteria → architect either revises instructions or escalates to user.
- Tester finds failing tests that developer can't fix in one honest attempt → architect to user.
- Security flags CRITICAL/HIGH → architect blocks, loops back to developer.
- Three rounds of developer ↔ tester without convergence → architect to user.

Don't infinite-loop the agents. If progress stalls, bring it to the user.
