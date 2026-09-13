---
name: shipmunk-task-board
description: "Execute Shipmunk work from the central GitHub Project and report concise, useful progress to its issue. Use when implementing, continuing, or handing off Shipmunk roadmap work."
---

# Shipmunk Task Board

The central tracker is [`ianrodrigues/shipmunk-tasks`](https://github.com/ianrodrigues/shipmunk-tasks) and its [Shipmunk Project](https://github.com/users/ianrodrigues/projects/1). Code belongs in the relevant implementation repository.

## Start

1. Read the assigned issue, labels, milestone, Project fields, and native dependencies. Do not begin blocked work.
2. Treat the issue’s goal, scope, acceptance criteria, and validation as the brief. Inspect the target repository’s instructions and current code before choosing an implementation.
3. Use a focused branch and pull request in the implementation repository. Link it to the central issue.

## Deliver

- Keep code, tests, and detailed durable evidence in the implementation repository.
- Run the issue’s stated validation and distinguish fixture results from live validation.
- Treat live validation as its own linked task when it needs designated infrastructure, credentials, coordination, or a separate pass/fail outcome. Keep ordinary validation as an acceptance or verification bullet in the implementation task.

## Report

When updating the central issue, write only:

- implementation link;
- validation result and fixture/live boundary;
- remaining blocker or next step.

Do not paste logs, test matrices, command transcripts, frontmatter, credentials, provider output, or historical narrative. Use native GitHub fields, labels, milestones, and dependencies for metadata; do not recreate local task IDs or Markdown task files.
