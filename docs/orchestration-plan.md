# Plan orchestration for agentbox

## Decisions
| # | Decision |
|---|----------|
| 1 | Approved tasks merge into integration branch `plan/<name>`; one final review + merge to main |
| 2 | Dependents branch from the integration branch tip |
| 3 | Manual sign-off by default; per-task `auto_approve`, set during plan review (before execution) |
| 4 | Interactive agents (attachable) |
| 5 | Done = agent writes a marker file (prompt-injected callback) |
| 6 | No verify command; users model tests as their own tasks |
| 7 | `max_parallel` config, default 3 |
| 8 | Merge conflict on approval -> auto-start a resolver agent |
| 9 | Rejection/failure blocks the task and dependents until the human intervenes |
| 10 | `.agentbox/plan.json` committed; separate git-ignored state file |
| 11 | `agentbox plan "goal"` runs a planner box; human reviews/edits the plan before anything runs |
| 12 | Milestone includes auto-start and the planner agent |

## Data model
plan.json: `{name, tasks:[{id,title,prompt,depends_on:[id],auto_approve,net?}]}`.
state (`.agentbox/plan-state.json`, git-ignored): per task `status`
(pending|ready|running|review|merging|conflict|accepted|blocked), `box`,
`attempts`, `updated`. Reconcile on start from state + box list + marker files.

## Behavior
- ready = all deps accepted and under max_parallel; scheduler starts boxes from integration tip.
- Prompt template appends: task, plan context, "when finished write /workspace/.agentbox-done (summary)". Orchestrator polls the marker in the snapshot.
- On done: `review` (human diff + approve/reject) unless auto_approve, which merges immediately.
- Approve: merge branch into `plan/<name>` (reusing abort-safe merge). Conflict: spawn resolver box on the integration branch with conflict context; its done marker completes the merge; if it fails, block.
- Reject/failure/agent exit without marker: `blocked`; human can resume the box with feedback, retry, or skip.
- All tasks accepted: plan complete; final review/merge of integration branch to main.

## Milestones
1. Plan package: schema, validation (cycles, unknown ids, dup ids), state file, pure scheduler (`ready(plan,state,max)`) with table tests.
2. Integration branch ops in box_ops: create/ensure `plan/<name>`, createBox from a base ref, merge into integration branch, final merge.
3. Prompt template and done-marker detection; box start with injected prompt.
4. Orchestrator loop (reconcile, auto-start, review transitions, resolver agent), CLI: `plan check|run|status`.
5. TUI: plan screen (tree, status colours, parallel groups, review inbox, toggle auto_approve in plan review, attach to task, approve/reject/skip/retry).
6. Planner: `agentbox plan "goal"` planner box prompt, validation, handoff to the plan review screen.
7. README/AGENTS docs, tests (scheduler, merge/resolver with fake runtime), config `max_parallel`.

## Risks / notes
- auto_approve without verify merges anything the agent marks done; plan review UI should warn.
- Marker is stochastic: show a "mark done manually" action and detect container exit without marker.
- Resolver-agent loops: cap at one attempt per merge.
