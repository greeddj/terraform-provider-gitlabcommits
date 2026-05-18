---
description: Run the full multi-agent workflow on the current task (architect → developer → tester → security → techwriter).
argument-hint: [task description]
---

This is the same as `/architect` but signals explicit intent to run the **full loop** (no shortcuts). Use when the user wants belt-and-braces: design review, implementation, tests, security audit, and docs all in one shot.

1. Invoke the **architect** with `$ARGUMENTS` plus all current context. Pass an explicit "run the full loop, do not skip steps" directive.
2. If the harness allows it, the architect delegates to other agents directly. Otherwise the architect returns dispatch instructions and the main thread runs each subagent in turn, feeding results back.
3. Stop only when:
   - the architect rejects the plan with a technical reason, OR
   - the architect returns a final summary marked **Decision: Accepted**.

The contract for the final report is:

```
## Architect summary
- Goal: ...
- Decision: Accepted | Accepted with scope cut | Rejected
- Changes landed: <path:line — what>
- Verification: just check / just test / coverage / security
- Residual risk / follow-ups
```

If anything is missing from that report, ask the architect for it before reporting "done" to the user.
