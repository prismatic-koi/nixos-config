# Invariant: the security threat model for sandboxed workers

This model applies to every sandboxed worker, in every isolation mode. A
review agent judges each security finding against this model. The model
decides two things:

- whether a finding is a blocker
- whether the finding is a known limit

## Attacker

The attacker is a worker agent. The worker is confused, or it obeys
instructions that are planted in the content that it reads. That content
can be an issue, a web page, or a file.

Assume that the attacker can obey a detailed recipe. The attacker can do
these actions:

- write any file that its sandbox permits
- run any command in its sandbox
- call the host API

## In scope

A finding is in scope when the worker can cause one of these results, and
no step needs an unusual action from the owner:

- code runs on the host, outside all sandboxes
- the worker reads a credential that it was not given
- the worker changes another session, the coordinator, or the `main`
  branch

A normal owner action is a usual action. These are usual actions:

- a prism command
- a look at a session
- the shell prompt
- a merge
- a `git pull`

The length of the chain does not decide scope. A planted recipe can be
long. Scope depends on who does the final step. If the worker does the
final step, the finding is in scope.

## Out of scope

Record each of these findings as a known limit. Do not report it as a
blocker. Do not open an issue for it.

- Code that runs only because the owner runs the project code of the
  worker. Examples are a build, a test, a language server, and a script in
  the repo.
- Denial of service. Examples are a worker that breaks its own session,
  holds a lock, or fills a disk.
- A result that needs the owner to do an unusual step by hand.

## Known limit

A known limit is one of these findings:

- an out-of-scope finding
- an in-scope finding that has a fix in a tracked issue for later work

Record each known limit in one line in the related invariant doc. The line
names the vector. The line also names the reason that the finding is out of
scope, or the issue that tracks the fix.
