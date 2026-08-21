# Follow-ups

Work that is deliberately not done yet, with the reasoning, so it can be
picked up without re-deriving it. Written 2026-08-21, after the ADK
migration audit.

---

## 1. Pause and resume — the big one

**The problem today.** When the agent wants to write a file or run a
command, ask pops a modal and the agent freezes. You have to be at that
machine, at that moment. Walk away and it sits there. Close the app and
the work is gone.

Worse: workflow tabs **auto-deny every approval**, because no human is
attached to answer. That is why workflows only really work for
pre-cleared operations.

**What changes.** The agent stops cleanly, saves where it was, and the
approval becomes a message instead of a blocking popup. You answer
whenever. The run picks up where it left off.

**What it unlocks, in rough order of value:**

1. **Workflows that can ask.** A pipeline that hits an approval pauses
   and waits instead of being denied. This is the difference between
   "workflows only do pre-cleared work" and "workflows do real work."
2. **Approvals that survive a restart.** Close the laptop, come back,
   approve, continue.
3. **Approve from somewhere else.** Once an approval is data rather than
   a modal, it can go to a phone, a web page, Slack. Prerequisite for ask
   running anywhere but the terminal in front of you.
4. **Long-running agents.** Something that runs for an hour, hits one
   decision point, parks, waits.

**Why it is one project, not two.** Resumable workflows and
non-blocking approvals need the same machinery: the ability to pause a
turn and pick it up later. Doing either gets most of the other.

**The ADK pieces that map to it:** `workflow.Persistence` and
`Workflow.Resume` for the workflow half, `NewRequestInputEvent` for
surfacing the prompt, and `tool/toolconfirmation` for approvals. ask
currently uses none of them, and `ToolEnv.ApprovalDenied` blocks on the
modal instead.

**Size.** Not small. ask has no suspend/resume path for a chat turn at
all; that is the work.

---

## 2. Parallel / fan-out

Independent of the above. Add a `parallel` step kind alongside `loop` in
a workflow definition: fan several steps out at once, join their results,
continue.

The graph engine already supports it — `JoinNode` for the join,
`NodeConfig.ParallelWorker` for running a node once per item of a list
input. The work is the workflow schema, the builder UI, and the
compiler mapping. It needs nothing from follow-up 1.

---

## 3. Agent discoverability (maybe)

**The symptom to watch for:** the model not using agents that are
defined. Today there is one `task` tool and the model names which agent
it wants as a parameter; the available agents are described in prose in
the system prompt.

ADK's `tool/agenttool` would make every agent its own tool, which models
pick from more reliably than from a prose list. It was rejected —
see "Deliberate departures" in `adk-20-upgrade.md` — because it would
cost every agent's definition on every request, and because subagents
would lose memory access, tool-error recovery, and background execution.

If this turns out to be a real complaint, cheaper fixes first: better
trigger descriptions on agent definitions, or surfacing agents through
the same registry search that MCP tools use.
