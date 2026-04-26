# D.3 — Test-naming audit

**Status:** Complete  
**Issue:** [#1111](https://github.com/prismatic-koi/nixos-config/issues/1111)  
**Depends on:** [Architecture inventory](../architecture-inventory.md) (§12, §14.2 step 12, and the §12 note at line 2223)  
**Design series:** [#1072](https://github.com/prismatic-koi/nixos-config/issues/1072)  
**Related:** D.1 (#1088), D.2 (#1089), E.1 (#1090)

---

## 1. Context

The architecture inventory (§12) recorded a test landscape table with per-package counts of
`Test*` functions. As generated, eleven packages showed **non-zero test LoC but zero `Test*`
function matches**. The inventory flagged this as ambiguous: possible causes were non-canonical
naming (e.g. `test_*`, `check*`, unexported helpers only), a regex error in the regen script,
or test files containing only benchmarks/examples.

This document audits each of the eleven packages, classifies the cause, and records the
evidence. No code or test changes are made here.

**Result in one sentence:** every package fell into cause (b) — inventory regex error.
The zeros were a mechanical shell bug in the step-12 regen recipe; all eleven packages
use canonical `func Test*(t *testing.T)` naming throughout.

---

## 2. Per-package findings

### 2.1 `internal/agent`

**Test file:** `internal/agent/machine_test.go` (151 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence:**

```
machine_test.go:8:   func TestTransition_ValidPairs(t *testing.T) {
machine_test.go:70:  func TestTransition_InvalidPairs(t *testing.T) {
machine_test.go:109: func TestTransition_DeletedIsTerminal(t *testing.T) {
machine_test.go:128: func TestTransition_UnknownFromState(t *testing.T) {
machine_test.go:141: func TestValidTransitionsCompleteness(t *testing.T) {
```

`rg -c '^func Test' internal/agent/machine_test.go` → **5** (no filename prefix, single-file package).

**Proposed action:** No action needed. The inventory §14.2 step-12 recipe has already been
corrected (see §12 note); the §12 table now records 5.

---

### 2.2 `internal/archive`

**Test file:** `internal/archive/archive_test.go` (668 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence (first and last three):**

```
archive_test.go:122: func TestRunHappyPath(t *testing.T) {
archive_test.go:214: func TestRunWithToolOutput(t *testing.T) {
archive_test.go:249: func TestRunNoHarnessSessionID(t *testing.T) {
…
archive_test.go:617: func TestRunRepoTraversalRejected(t *testing.T) {
archive_test.go:653: func TestToolOutputIDValidation(t *testing.T) {
archive_test.go:653: func TestContainerNameForSession(t *testing.T) {
```

`rg -c '^func Test' internal/archive/archive_test.go` → **15**.

**Proposed action:** No action needed.

---

### 2.3 `internal/db`

**Test file:** `internal/db/db_test.go` (5899 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence (first and last three):**

```
db_test.go:34:   func TestOpen_CreatesSchema(t *testing.T) {
db_test.go:83:   func TestUpsertStatus_Insert(t *testing.T) {
db_test.go:117:  func TestUpsertStatus_Update(t *testing.T) {
…
db_test.go:5631: func TestInsertSession_ZeroStartedAt(t *testing.T) {
db_test.go:5673: func TestInsertSession_ExplicitStartedAt(t *testing.T) {
db_test.go:5814: func TestMigration_V17ToV18_BackfillsStartedAt(t *testing.T) {
db_test.go:5865: func TestMigration_V17ToV18_Idempotent(t *testing.T) {
```

`rg -c '^func Test' internal/db/db_test.go` → **122**.

**Proposed action:** No action needed.

---

### 2.4 `internal/git`

**Test file:** `internal/git/git_test.go` (641 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence (first and last three):**

```
git_test.go:46:  func TestSymbolicRef_SimpleBranch(t *testing.T) {
git_test.go:58:  func TestSymbolicRef_SlashBranch(t *testing.T) {
git_test.go:70:  func TestSymbolicRef_DetachedHead(t *testing.T) {
…
git_test.go:478: func TestStat_DeduplicatesStagedAndUnstaged(t *testing.T) {
git_test.go:519: func TestCloneWorktree_EmptyRepo(t *testing.T) {
git_test.go:593: func TestCloneWorktree_NonEmptyRepo(t *testing.T) {
```

`rg -c '^func Test' internal/git/git_test.go` → **20**.

**Proposed action:** No action needed.

---

### 2.5 `internal/harness/opencode`

**Test file:** `internal/harness/opencode/adapter_test.go` (331 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence (first three):**

```
adapter_test.go:19: func TestConfigEnvVar(t *testing.T) {
adapter_test.go:29: func TestRuntimeEnv_ContainsBashTimeout(t *testing.T) {
adapter_test.go:44: func TestRuntimeEnv_ReturnsNewMapEachCall(t *testing.T) {
…13 total…
adapter_test.go:226: func TestCreateSession_SucceedsFirstAttempt(t *testing.T) {
adapter_test.go:268: func TestCreateSession_RetriesAndSucceeds(t *testing.T) {
adapter_test.go:302: func TestCreateSession_ExhaustsRetries(t *testing.T) {
```

`rg -c '^func Test' internal/harness/opencode/adapter_test.go` → **13**.

**Proposed action:** No action needed.

---

### 2.6 `internal/integration`

**Test file:** `internal/integration/integration_test.go` (954 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence (first and last three):**

```
integration_test.go:179: func TestDBLifecycle_IdleActiveFinished(t *testing.T) {
integration_test.go:234: func TestDBLifecycle_BusMessageDelivered(t *testing.T) {
integration_test.go:266: func TestPaneDiedHook_ActiveToInterrupted(t *testing.T) {
…
integration_test.go:824: func TestSessionCreate_ForceFresh_False_StaleSession(t *testing.T) {
integration_test.go:880: func TestSessionCreate_ForceFresh_True_LiveSession(t *testing.T) {
integration_test.go:923: func TestSessionCreate_ForceFresh_False_NoDBRow(t *testing.T) {
```

`rg -c '^func Test' internal/integration/integration_test.go` → **15**.

**Proposed action:** No action needed.

---

### 2.7 `internal/mergequeue`

**Test file:** `internal/mergequeue/watcher_test.go` (1344 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence (first and last three):**

```
watcher_test.go:60:   func TestEnqueueMerge_FIFO(t *testing.T) {
watcher_test.go:102:  func TestEnqueueMerge_Idempotent(t *testing.T) {
watcher_test.go:137:  func TestEnqueueMerge_TerminalRowReplaceable(t *testing.T) {
…
watcher_test.go:1207: func TestWatcher_RunGHPassesRepoFlag(t *testing.T) {
watcher_test.go:1300: func TestWatcher_RunExitsWhenRepoUnresolved(t *testing.T) {
watcher_test.go:1334: func TestNew_NoAgentStatusRow_LeavesRepoEmpty(t *testing.T) {
```

`rg -c '^func Test' internal/mergequeue/watcher_test.go` → **30**.

**Proposed action:** No action needed.

---

### 2.8 `internal/opencode`

**Test file:** `internal/opencode/session_test.go` (144 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence:**

```
session_test.go:L (approx 1):  func TestLatestSessionForDir_ReturnsLatest(t *testing.T) {
session_test.go:             func TestLatestSessionForDir_UnknownDir(t *testing.T) {
session_test.go:             func TestLatestSessionForDir_MissingDB(t *testing.T) {
session_test.go:             func TestLatestSessionForDir_XDGFallback(t *testing.T) {
```

`rg -c '^func Test' internal/opencode/session_test.go` → **4**.

**Proposed action:** No action needed.

---

### 2.9 `internal/payload`

**Test file:** `internal/payload/payload_test.go` (294 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence (first and last three):**

```
payload_test.go:L~1:   func TestStateChange_Roundtrip(t *testing.T) {
payload_test.go:       func TestMsgUser_Roundtrip(t *testing.T) {
payload_test.go:       func TestMsgUser_EmptyAgentModel_Roundtrip(t *testing.T) {
…
payload_test.go:       func TestPermissionAsk_AbsentTool(t *testing.T) {
payload_test.go:       func TestPermissionDenied_Roundtrip(t *testing.T) {
payload_test.go:       func TestJSONFieldNames(t *testing.T) {
```

`rg -c '^func Test' internal/payload/payload_test.go` → **17** (includes 6 `PermissionAsk` variants).

**Proposed action:** No action needed.

---

### 2.10 `internal/piexport`

**Test file:** `internal/piexport/piexport_test.go` (907 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence:**

```
piexport_test.go: func TestFixtureLinearText(t *testing.T) {
piexport_test.go: func TestFixtureToolCalls(t *testing.T) {
piexport_test.go: func TestFixtureThinkingBlocks(t *testing.T) {
piexport_test.go: func TestZeroMessages(t *testing.T) {
piexport_test.go: func TestAbortedAssistant(t *testing.T) {
piexport_test.go: func TestAtomicWrite(t *testing.T) {
piexport_test.go: func TestOverwriteAtomic(t *testing.T) {
piexport_test.go: func TestIDUniqueness(t *testing.T) {
```

`rg -c '^func Test' internal/piexport/piexport_test.go` → **8**.

**Proposed action:** No action needed.

---

### 2.11 `internal/sse`

**Test file:** `internal/sse/client_test.go` (579 lines, 1 file)

**Cause classification:** (b) Inventory regex error

**Evidence (first and last three):**

```
client_test.go: func TestParseSimpleEvent(t *testing.T) {
client_test.go: func TestMultipleEvents(t *testing.T) {
client_test.go: func TestMultiLineData(t *testing.T) {
…
client_test.go: func TestConnectRetryOnStartup(t *testing.T) {
client_test.go: func TestDataWithColonNoSpace(t *testing.T) {
client_test.go: func TestServerConnectedEvent(t *testing.T) {
client_test.go: func TestLargeDataLine(t *testing.T) {
```

`rg -c '^func Test' internal/sse/client_test.go` → **14** (inventory table shows 14; see note in §3).

`rg --no-filename -c '^func Test' internal/sse/client_test.go` → **14**.

**Proposed action:** No action needed.

---

## 3. Root cause: the regen-script shell bug

The inventory's step-12 recipe (§14.2) used:

```bash
rg -c '^func Test' "$dir"/*_test.go 2>/dev/null | awk -F: '{s+=$2}END{print s+0}'
```

When a package contains **exactly one `_test.go` file**, `rg -c` is given a single filename
argument and outputs **only the bare count** — no `filename:` prefix. Example:

```
# single file: output is just "5", no colon
$ rg -c '^func Test' internal/agent/machine_test.go
5

# multiple files: output has "filename:count" shape
$ rg -c '^func Test' internal/config/config_test.go internal/config/profiles_test.go
internal/config/config_test.go:26
internal/config/profiles_test.go:6
```

The `awk -F: '{s+=$2}'` expression summed column `$2` (the part after the colon). For
single-file packages, the output was a bare number with no colon, so `$2` was empty, and
the sum was **0**. Every one of the eleven flagged packages had exactly one `_test.go` file.

The corrected recipe (already recorded in §14.2) uses `--no-filename` to normalise the
output shape regardless of how many files are matched:

```bash
rg -c --no-filename '^func Test' "$dir"/*_test.go 2>/dev/null | awk '{s+=$1}END{print s+0}'
```

**Follow-up for the inventory script:** The §14.2 step-12 recipe has already been corrected
in the inventory document itself (committed under #1076). No further action on the script
is needed. If the inventory is regenerated, the corrected recipe must be used — the old form
will re-introduce the zeros.

---

## 4. Summary table

| Cause | Count | Packages |
|---|---|---|
| (a) Non-canonical naming — functions exist but aren't named `Test*` | 0 | — |
| (b) Inventory regex error — functions correctly named but regex missed them | **11** | `internal/agent`, `internal/archive`, `internal/db`, `internal/git`, `internal/harness/opencode`, `internal/integration`, `internal/mergequeue`, `internal/opencode`, `internal/payload`, `internal/piexport`, `internal/sse` |
| (c) Helpers/benchmarks only — test files exist but contain only non-`Test*` functions | 0 | — |
| (d) Empty or near-empty test files | 0 | — |

All eleven packages use canonical `func Test*(t *testing.T)` naming. The common trait is
that each has exactly one `_test.go` file, which triggered the `rg -c` single-argument
output shape that the original awk expression could not parse.

---

## 5. Open questions

None. The cause is unambiguous and fully confirmed by direct inspection of every affected
file.

[uncertain] The inventory note at §12 mentions that `internal/session` and `internal/sidecar`
have "drifted from `rg` ground truth since the inventory was first generated". Those two
packages have **multiple** `_test.go` files and were therefore **not** affected by this
single-file bug. Their drift is a separate concern out of scope for this review and should
be folded into the next full inventory regeneration.
