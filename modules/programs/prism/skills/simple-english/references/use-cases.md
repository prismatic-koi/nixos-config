# Use cases beyond documentation

STE was built for aircraft maintenance manuals. The same properties — one
meaning per word, short sentences, condition before command — transfer to
any text where a misreading carries a cost. By the own count of the
standard, a majority of registered STE users work outside aerospace and
defence.

Each case below names the mode and the adaptations.

## Error messages and CLI output

Mode: procedural. This is the highest-value target. An error message is an
instruction to a stressed reader at any hour.

Pattern: state what happened, in simple past. State the cause, if known.
Give the command or the condition that fixes it.

Before: "Oops! Something went wrong while attempting to establish a
connection. Please ensure your credentials are properly configured and try
again."
After: "Connection to the database failed. The password for user `app` was
not correct. Set `DB_PASSWORD` and connect again."

## Runbooks and standard operating procedures

Mode: strict-leaning procedural. This is the home ground of STE. An
on-call runbook is a maintenance manual.

- Every step takes the imperative. One instruction per step. Conditions
  come first.
- A warning comes before the step. Command first, risk second.
- Enforce the 20-word limit without exception. An operator under pager
  stress reads each sentence once.

## Incident reports and postmortems

Mode: descriptive. Simple past only. A timeline written in present perfect
("we have identified") hides when events happened.

Before: "We have identified an issue that may have impacted some users'
ability to access the service."
After: "Between 14:02 and 14:31 UTC, 12% of requests failed. A deploy at
14:00 removed the cache warmup step."

STE bans a hedge such as "may have impacted". The report states what is
known, and states "unknown" for the rest. The result reads as more honest,
because it is.

## Commit messages and PR descriptions

Mode: descriptive body, imperative subject line. This convention already
matches STE: an imperative subject line, plain past-tense facts in the body.
Apply the substitution table and the 25-word limit to the body. Delete
phrases such as "this PR aims to".

## API changelogs and release notes

Mode: descriptive. One entry states one change, in one sentence where
possible. A "Breaking:" entry follows the warning pattern, command first:
"Update your calls to `v2/users`. The `name` field split into `first_name`
and `last_name`."

## Instructions for AI agents (prompts, AGENTS.md files, skills)

Mode: procedural. A system prompt is a procedure for a reader with no
ability to ask a question, the exact reader STE was built for.

- One instruction per sentence keeps each rule independently quotable, and
  hard to half-follow.
- One word for one meaning stops a model from treating "check", "verify",
  and "validate" as three separate operations.
- Condition first ("If the build fails, stop") beats a trailing condition,
  which a model tends to drop.
- No banned modal. A model reads `should` as optional guidance. Write
  `must`, or delete the rule.

## Support macros and status-page updates

Mode: descriptive, 25-word limit. A non-native reader makes up a large share
of many user bases. Do not write "we sincerely apologize for any
inconvenience this may have caused". Write: "The API was down for 18
minutes. Uploads made during this time were saved, and process today."

## Translation and localisation preparation

Mode: strict. The original purpose of STE was to make English readable
for non-native maintenance crews. The same property serves as pre-editing for
machine translation. One meaning per word, plus complete grammar (articles,
"that"), removes most translation ambiguity. Where a document gets
localised, STE lowers the error rate and the cost.

## UI copy and empty states

Mode: procedural, with a hard length limit. A button and a label are
technical names, and are exempt. Body copy follows the rules: "No projects
yet. Create a project to start." At this length, nothing else survives
regardless.

## Where STE does not fit

Marketing pages, launch posts, blog voice, and brand writing. STE removes
persuasion by design. Write that content in your own voice, then apply STE
to the docs the landing page links to.
