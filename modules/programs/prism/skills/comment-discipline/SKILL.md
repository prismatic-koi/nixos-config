---
name: comment-discipline
description: Load this skill when you write or review a code comment — before adding a comment during implementation, or when a reviewer judges whether an existing comment earns its place. Covers the five comment-discipline rules and the deletion test that decides whether a comment stays.
---

# Comment discipline: write comments that earn their place

Comments proliferate across repos: verbose, historical, and restating the
obvious. Each one is a context-management cost. Agents and humans burn
tokens and attention reading bloated files, and the bloat compounds on
every edit. This skill states the rules that stop the bloat at the source
and give reviewers a clear pass/fail test.

## The five rules

1. **STE.** Write comments in Simplified Technical English: short
   sentences, one idea each, plain words. Load the `simple-english` skill
   for the mechanics. Do not restate that catalogue here.
2. **Current state only.** A comment explains WHY a non-obvious decision
   holds now. Do not record history inside a comment — no "was X",
   "changed from Y", "removed Z". Git owns the history.
3. **No ghosts.** Do not write a comment about a thing that is gone or a
   thing that does not exist yet. A comment describes the code next to it,
   not a past version or a planned version. Exception: a comment about a
   dead thing that guards against its return is not a ghost. State the
   guard as a present-tense condition, not as history. Example: write "If
   the handler writes the raw name, this test fails", not "Before #1234
   the handler wrote the raw name; this test caught it".
4. **No restatement.** Do not narrate what the code plainly does. If a
   reader can see the fact from the code itself, the comment adds no
   information.
5. **The deletion test.** Delete the comment. Ask: can this deletion cause
   a wrong decision, or let someone reintroduce a bug? If not, delete the
   comment. This is the pass/fail rule below — apply it to every comment
   you write or review.

## The deletion test as pass/fail

Apply the deletion test to decide whether a comment stays:

- **Delete it if:** removing the comment leaves the reader with the same
  correct understanding, and no one can reintroduce a bug by not knowing
  the fact the comment stated.
- **Keep it if:** removing the comment removes knowledge a reader needs to
  avoid a wrong decision — a non-obvious constraint, a safety rule, or a
  reason that is not visible in the code itself.

## Worked examples

### Survives the deletion test — load-bearing safety notes

```python
# Do not "fix" this spelling: `custmer_id` is the real column name in prod.
custmer_id = row["custmer_id"]
```

Delete this comment and a future editor can rename the column to the
correct spelling, breaking every query against the real table. The
comment carries information the code cannot show by itself. Keep it.

```go
// Fail shut: if the policy fetch errors, deny the request. Do not default
// to allow.
if err != nil {
    return deny
}
```

Delete this comment and a future editor can plausibly swap the branch
to fail open, on the reasonable-looking assumption that erroring should
not block traffic. The comment states a security-relevant intent that
the code alone does not carry. Keep it.

### Fails the deletion test — delete these

```python
# Loop over the list of users and print each one.
for user in users:
    print(user)
```

The code says exactly this. Deleting the comment loses nothing. Delete it.

```go
// Changed from a map to a slice on 2024-03-02 because the map was slow.
// Previously this held a map[string]int.
items := []Item{}
```

This is history, not a current-state fact. Git already carries this
information (`git log`, `git blame`). Deleting the comment loses nothing a
reader needs today. Delete it.

```python
# TODO: remove this once the old auth service is retired.
# The old auth service was retired in June; this codepath is now unused.
```

This is a ghost: a comment about a thing that is already gone. If the
codepath is genuinely unused, delete the codepath, not just the comment.
If it is still in use for another reason, write a current-state comment
that says why it still runs.

## For reviewers

When you judge a comment during review, load this skill and apply the
deletion test above. This is a judgment call, not a nitpick machine: raise
it only when a comment genuinely bloats a file — long, historical, or
narrating the obvious — not for every comment a reviewer would have
phrased differently.
