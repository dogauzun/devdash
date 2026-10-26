# The devdash Tasks database

Reference and recipes for agents that file, query and move devdash tasks in Notion. The binding rules (statuses and
transitions, task keys and the `PR` property) are in `CLAUDE.md` under Working rules; this file does not repeat them.

Work has been tracked in Notion since 2026-10-26. `DEV-n` keys in git history, `DECISIONS.md` and code from before
the cutover refer to the closed Jira project DEV, not to the IDs in this database.

## Database

- devdash Tasks: database `2cbd07b5e12245b1b640b5e67fb94f0b`, data source
  `collection://f734c0d6-d1bd-4525-80ec-e56461f5d053`, ID prefix `DEV`.
- Properties: `Name` (title); `ID` (unique id, read-only; SQL column `"userDefined:ID"`); `Status` (status: To Do,
  In Progress, In Review, Done; default To Do); `Type` (select: Task, Bug, Epic); `Labels` (multi-select:
  `needs-decision`, `blocked`, `qa`, `docs`, `code-quality`, `security`, `build`); `Epic` / `Children` (self-relation);
  `Blocked by` / `Blocks` (self-relation); `PR` (url); `Updated` (last edited time).
- Views: Board (by Status, epics hidden), Needs decision, Blocked, Open epics, Done this week.

## Tickets

- A ticket's body has the sections `## What`, `## Scope`, `## Acceptance`, `## Depends on` and `## Notes`. It is in
  English, holds one day of work at most and uses synthetic figures only.
- Epics are containers, never work items; a task hangs under its epic through `Epic`.
- A dependency is a `Blocked by` relation plus the Depends on text.
- `needs-decision` means a question waits for the owner; `blocked` means the owner has not released the task. Sweeps
  skip both.
- There are no estimates and no effort record: the status is the only progress signal.
- The ID is a handle for chat, ledgers and searches only.
- A transition is `update-page` with `update_properties` and the status name.
- A duplicate is closed with a comment naming the task that survives, and status Done.
- After a filing wave, check for duplicates with the query below and look for repeated names. Whether `create-pages`
  can file a task twice on a silent retry is not known.
- GitHub issues stay the entry point for outside contributors; a maintainer re-files an accepted issue as a task and
  links the two both ways.
- Any agent may file and move its own tasks. On 2026-10-26 a general-purpose subagent created a task, read it back
  through SQL, made the three transitions, set `PR` and deleted the task with no permission or classifier denial, so
  the Jira-era rule that only the controller touches the tracker no longer applies.

## SQL recipes

Queries go through the Notion MCP tool `query-data-sources` in mode `sql`, with the data source URL in
`data_source_urls` and, in square brackets, as the table.

Board, everything not done:

```sql
SELECT "userDefined:ID" AS id, Name, Status, Type, Labels, Epic, PR, url
FROM [collection://f734c0d6-d1bd-4525-80ec-e56461f5d053]
WHERE Status != 'Done'
ORDER BY "userDefined:ID"
```

The task of a PR (the PR body carries no task link; the `PR` property is the one link between the two):

```sql
SELECT "userDefined:ID" AS id, Name, Status, url
FROM [collection://f734c0d6-d1bd-4525-80ec-e56461f5d053]
WHERE PR = 'https://github.com/dogauzun/devdash/pull/<n>'
```

To Do sweep, the tasks ready to start: the board query with these conditions added to its `WHERE`. Relation columns
such as `Blocked by` can be queried only after the data source has been fetched in the session.

```sql
AND Status = 'To Do' AND Type != 'Epic'
AND (Labels IS NULL OR (Labels NOT LIKE '%needs-decision%' AND Labels NOT LIKE '%blocked%'))
AND ("Blocked by" IS NULL OR "Blocked by" = '[]')
```

Decisions sweep, what waits for the owner:

```sql
SELECT "userDefined:ID" AS id, Name, Status, Labels, url
FROM [collection://f734c0d6-d1bd-4525-80ec-e56461f5d053]
WHERE Status != 'Done' AND (Labels LIKE '%needs-decision%' OR Labels LIKE '%blocked%')
```

Duplicates after a filing wave:

```sql
SELECT "userDefined:ID", Name
FROM [collection://f734c0d6-d1bd-4525-80ec-e56461f5d053]
WHERE "userDefined:ID" >= <first id of the wave>
ORDER BY Name
```

## Filing

A task is filed with the Notion MCP tool `create-pages`: the parent
`{"type":"data_source_id","data_source_id":"f734c0d6-d1bd-4525-80ec-e56461f5d053"}`, the properties `Name`, `Type`,
`Labels`, `Epic` (an array of page URLs) and `Blocked by` (an array of page URLs), and the ticket body as the content.
