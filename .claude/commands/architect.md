---
description: Hand off the current task to the architect agent for analysis, decision, and orchestration of developer / tester / security / techwriter.
argument-hint: [task description, e.g. "add execute_filemode support"]
---

Invoke the **architect** agent (opus) with the full context of `$ARGUMENTS` plus any conversation context that's relevant to the task. The architect will:

1. Read the relevant code and verify the request is sound.
2. Reject the plan with a technical reason if it's bad — or accept it.
3. Delegate to **developer**, then **tester**, then **security**, then **techwriter**, looping back as needed.
4. Return a structured summary of what changed, what was verified, and any residual risk.

Use the `Task` tool:

```
Task(
    description="Architect: <short task>",
    subagent_type="architect",
    prompt="<full self-contained brief: user goal, relevant file:line citations from current context, acceptance criteria, anything the user has already ruled out>"
)
```

If the architect rejects the plan, **do not work around it** — relay the rejection to the user with the technical reason and ask whether to adjust the goal.

See `.claude/skills/multi-agent-workflow/SKILL.md` for the full loop and stop conditions.
