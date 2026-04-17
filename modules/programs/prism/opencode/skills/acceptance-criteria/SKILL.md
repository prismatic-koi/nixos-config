---
name: acceptance-criteria
description: Load this skill when drafting or reviewing acceptance criteria for an issue or ticket — before creating a work item, preparing a worker spawn prompt, or critiquing existing ACs before accepting work.
---

# Acceptance Criteria

## When to load

Load this skill when:

- Drafting acceptance criteria for a new issue or ticket body.
- Preparing a spawn prompt — reviewing or finalising ACs before handing work to a worker agent.
- Critiquing ACs on an existing issue or ticket before accepting it as ready to action.

---

## Tag taxonomy

Each AC carries exactly one tag. The four valid tags are:

- **`[functional]`** — core behaviour the work item must exhibit. Use for primary happy-path outcomes: what the system does when everything goes right. Example: `- [ ] [functional] The CLI prints the resolved skill name and path when a skill loads successfully.`

- **`[security]`** — auth, authorisation, data exposure, injection surface. Use when the work touches permissions, credentials, external input that reaches a backend, or any surface where a malicious actor could extract or corrupt data. Example: `- [ ] [security] An unauthenticated request to /api/tokens returns 401 and no token data.`

- **`[edge-case]`** — boundary conditions, empty/null/missing inputs, error states. Use for inputs or states that are unusual but valid, inputs that should be rejected gracefully, and failure modes the implementation must handle. Example: `- [ ] [edge-case] Passing an empty string as the skill name returns a clear error message and exits non-zero.`

- **`[performance]`** — response time, throughput, resource constraints. Only include if the work item has a plausible performance dimension — do not add performance ACs by default. Example: `- [ ] [performance] The index query returns within 200 ms at p99 under the documented load profile.`

### Tags that must NOT appear in AC lists

- **`[regression]`** — this is a reviewer concern, not an acceptance criterion. Whether a change introduces a regression is verified by the test suite and the reviewer, not stated as an AC.
- **`[non-breaking]`** — compatibility guarantees are architectural constraints, not observable outcomes to check off. State the specific observable if one exists, and tag it `[functional]` or `[edge-case]`.
- **`[test]`** — ACs describe what is true about the system, not how it is verified. "Unit tests cover X" is a quality gate, not an AC.

---

## Rubric — the five checks

Run every AC through all five checks before finalising.

### 1. Falsifiability

Each AC must be verifiable as pass or fail without human judgement. An AC that requires subjective assessment is not an AC.

- **Anti-pattern:** "The output is valid JSON." (What counts as valid? The implementer may disagree.)
- **Fix:** "The output is parseable by `jq` without error and contains a `name` key at the top level."

### 2. Completeness

The AC set must cover the happy path, relevant edge cases, the security surface, and any build or runtime constraints implied by the work item. A gap in the AC set is an invitation to ship incomplete work.

- **Anti-pattern:** A feature that accepts user input has no `[edge-case]` ACs for malformed or empty input.
- **Fix:** For each distinct path described in the work item (including failure paths), confirm that at least one AC covers it.

### 3. Specificity

ACs name what is true about the system, not how to test it. An AC that describes a test procedure is misspecified.

- **Anti-pattern:** "Verify by running `make test` that the module loads." (Test procedure, not observable outcome.)
- **Fix:** "The module loads without error on startup." (Observable outcome; leave the test method to the implementer.)

### 4. Redundancy

Two ACs asserting the same observable, or one AC bundling multiple independent conditions, both dilute the checklist.

- **Anti-pattern:** `- [ ] [functional] Unit tests cover the happy path, error path, and empty-input case.` (Three conditions in one AC; also a `[test]` tag in disguise.)
- **Fix:** Split into separate ACs, one per observable outcome. Remove any AC that is trivially implied by another.

### 5. Tagging

Each AC carries exactly one tag from the four-tag taxonomy above. A mislabelled AC usually signals the condition has not been thought through.

- **Anti-pattern:** `- [ ] [regression] The existing CLI flags still work.` (Forbidden tag; also vague.)
- **Fix:** `- [ ] [functional] Existing CLI flags (`--branch`, `--prompt`) behave identically to before the change.`

---

## Format template

```
- [ ] [tag] Statement of observable outcome
```

Rules:
- One outcome per line.
- Do not combine two conditions into a single AC.
- Be specific: name endpoints, fields, status codes, error messages, flag names where relevant.
- Do not pad. Five precise ACs are better than ten vague ones.

### Worked example

Work item: "Add a `--dry-run` flag to the deploy command that prints what would be deployed without executing."

```
- [ ] [functional] Running `deploy --dry-run` prints the list of resources that would be deployed and exits 0.
- [ ] [functional] Running `deploy --dry-run` does not create, update, or delete any remote resources.
- [ ] [edge-case] Running `deploy --dry-run` with no resources to deploy prints "nothing to deploy" and exits 0.
- [ ] [edge-case] Passing `--dry-run` alongside `--force` produces a clear error message and exits non-zero.
- [ ] [functional] Running `deploy` without `--dry-run` is unchanged in behaviour.
```

---

## Writing mode

Use this workflow when no ACs exist yet and you are drafting them from scratch.

1. **Read the source in full.** Read the issue or ticket body, including scope notes, out-of-scope items, and any linked context. Do not produce ACs until you have read everything.
2. **Extract the proposed behaviour.** What does "done" look like from a user or system perspective? Write this in one or two sentences as a private working note — do not output it.
3. **Enumerate the paths.** For each distinct path implied by the work item, note it:
   - Happy path — the primary successful outcome.
   - Edge cases — unusual but valid inputs, boundary conditions, expected failure states.
   - Security surface — does the work touch auth, permissions, external input, or sensitive data?
   - Performance — only if the work has an obvious performance dimension.
4. **Draft one AC per observable outcome.** Each path from step 3 should produce at least one AC.
5. **Apply the rubric.** Run every drafted AC through the five checks (falsifiability, completeness, specificity, redundancy, tagging). Revise any that fail.
6. **Output the checklist.** Format using the template above. No preamble, no summary after the list.

---

## Reviewing mode

Use this workflow when ACs already exist on an issue or ticket and you are critiquing them before the work is actioned.

1. **Read the existing ACs alongside the work item.** The work item is your ground truth; the ACs are the claim being tested.
2. **Run each AC through the five checks.** Flag any AC that fails falsifiability, specificity, or uses a forbidden tag.
3. **Check for gaps.** For each path described in the work item (happy, edge, security, performance), confirm at least one AC covers it. Name any uncovered paths explicitly.
4. **Check for redundancy.** Identify ACs that duplicate each other or bundle multiple conditions.
5. **Produce a revised checklist.** Output the improved AC list using the format template. If the existing ACs are already sound, output them unchanged. Be direct about gaps — if a security AC is missing entirely, say so plainly before the revised list.

---

## Scope guidance — when to skip ACs

Skip ACs for:
- Single-line fixes (typo corrections, off-by-one patches with no branching behaviour).
- Documentation-only changes (README edits, comment updates, doc typos).
- Trivial config nudges (bumping a version number, changing a timeout value with no logic change).

When in doubt, write ACs. A short checklist for a small change takes two minutes and prevents scope drift.
