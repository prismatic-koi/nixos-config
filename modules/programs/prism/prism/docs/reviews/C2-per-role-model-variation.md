# C.2 — Per-role model variation proposal

**Track:** C (A/B testing) — Wave 2.
**Issue:** #1085.
**Source corpus:** inventory §8.7, §3.10, §3.16; design doc #1072; RFC #691 §5; sibling C.1 (#1075); `internal/harness/opencode/adapter.go`; `cmd/spawn.go`; `internal/review/review.go`; `internal/config/profiles.go`.
**Related:** C.1 (#1075, schema delta), C.3 (#1086, outcome capture), C.4 (#1087, prompt + skills).

---

## 1. Context recap

Inventory §8.7 summarises the per-axis variation surface. On the **model axis** specifically, it observes:

> Today `--model` / `--variant` apply at the spawn (and review-fan-out) level, not at the role level. To vary `@review-context with claude-opus-4-7 vs @review-context with gemini-2.5-pro` within one spawn would require either:
> - a per-role override map in the spawn flags (absent), or
> - an internal `harness.Harness.EffectiveModel(role)` implementation that consults a map keyed by role (the interface method exists; the opencode adapter resolves a single `agentModel` field passed at construction time).

Design doc #1072 (Track C framing) states the C.2 deliverable as: _propose the shape of a per-role override map covering the CLI surface, config-file surface, plumbing surface, and persistence surface_. RFC #691 §5 introduces `prism spawn --abtest` as the canonical user-facing surface for experimentation; per-role model variation is the lowest-granularity slice of that capability.

The C.1 proposal (#1075) laid out the `spawn_inputs` table schema and proposed a `model_variant_overrides` JSON column hook for C.2 to flesh out. This document fills that hook.

---

## 2. Current state

### 2.1 The single-model constraint

The opencode adapter is constructed with a single `agentModel` string:

```
internal/harness/opencode/adapter.go:67    New(opencodeURL, httpClient, agentRole, agentModel string) *Adapter
internal/harness/opencode/adapter.go:84    NewContainerMode(opencodeURL, httpClient, agentRole, agentModel string) *Adapter
internal/harness/opencode/adapter.go:49    // agentModel string — single field, one per adapter instance
```

`EffectiveModel(role string)` (`:640-665`) accepts a `role` parameter but does not consult a per-role map. It reads `opencode.json` from the host filesystem and returns the per-agent model field from it — i.e. it reflects opencode's own config, not a prism-managed map. The `role` parameter is used as a lookup key into opencode's `agent.<name>.model` config, not into anything prism constructs at spawn-time.

### 2.2 Profile system already has per-role structure

`internal/config/profiles.go` is further along than the adapter. `ProfileEntry` is `map[string]RoleConfig` where each entry is `{Model, Variant}`. `BuildConfigContent` (`profiles.go:230`) already emits `agent.<name>.model` and `agent.<name>.variant` entries per role. This means the **opencode config-file surface already supports per-role variation** — the gap is that prism has no way to pass per-role overrides *at spawn time* on top of a selected profile, and the adapter does not expose this to the harness interface.

### 2.3 Review fan-out uses a single model for all five agents

`internal/review/review.go` `Opts` (`:572-634`) has no `Model` field. Review agents are spawned with the profiles file's container config blobs (`ContainerReviewGoalConfig`, etc.) which are pre-baked by Nix. If a user wants to run `@review-context` with a different model from the other four review agents, there is currently no mechanism.

### 2.4 Proxy-spawn path gap

`internal/sidecar/sidecar.go:3184-3214` (the host-API proxy-spawn handler) passes `--model` when `req.Model != ""`. As C.1 noted (§2.1 proxy-spawn parity caveat), the proxy request struct does not carry `--variant` or a per-role override map. Any per-role override mechanism will need to be added to the proxy path as well as the CLI path.

---

## 3. Four-surface analysis

### 3.1 CLI surface

**The problem.** `cmd/spawn.go:144` defines `--model` as a single string flag that overrides all agents' model or just the primary-role agents (depending on whether `--profile` is also set, via `BuildConfigContent` at `:318`). There is no mechanism to express "use model A for `@review-context` and model B for all others."

**Three candidate shapes** are analysed in §4.

### 3.2 Config-file surface

The `profiles.json` file (`ProfilesFile` in `internal/config/profiles.go`) already has the right shape for per-role variation at the *profile* level:

```json
{
  "profiles": {
    "anthropic": {
      "primary":     {"model": "anthropic/claude-opus-4-6"},
      "secondary":   {"model": "anthropic/claude-sonnet-4-6"},
      "lightweight": {"model": "anthropic/claude-haiku-4-5"}
    }
  },
  "role_mapping": {
    "primary":     ["coordinator", "plan"],
    "secondary":   ["worker", "review", "ac"],
    "lightweight": ["explore", "title", "summary", "compaction"]
  }
}
```

**What it cannot express today.** The role_mapping groups agents into buckets (primary / secondary / lightweight). It cannot express a per-named-agent override like `{"review-context": "google/gemini-2.5-pro"}` without either:

1. Adding a new role bucket that contains only `review-context`, or
2. Adding a dedicated `agent_overrides` map parallel to the role-bucket structure.

Option 1 is a Nix-side change and would work cleanly but is coarse — it forces the user to define a whole new Nix profile just to change one agent's model. Option 2 is more surgical.

**Proposed extension.** Add an optional `agent_overrides` map to `ProfileEntry`:

```json
{
  "profiles": {
    "anthropic-opus-context": {
      "primary":     {"model": "anthropic/claude-opus-4-6"},
      "secondary":   {"model": "anthropic/claude-sonnet-4-6"},
      "lightweight": {"model": "anthropic/claude-haiku-4-5"},
      "agent_overrides": {
        "review-context": {"model": "google/gemini-2.5-pro"}
      }
    }
  }
}
```

This is purely additive. `BuildConfigContent` would apply `agent_overrides` after the role-bucket expansion, letting named agents override the bucket default. The existing profile format is unchanged for profiles without `agent_overrides`.

**Alternatively**, per-role overrides may be expressed entirely at the CLI (see §4) with no config-file change needed; the config-file extension is only necessary if users want the variation encoded in a named profile rather than spelled out on every invocation.

### 3.3 Plumbing surface

The data path from flag → adapter is:

```
cmd/spawn.go:60,110,143-145   reads --model → StartSidecarOpts (via session.SpawnOpts)
session/spawn.go:623           SpawnOpts → StartSidecarOpts{..., Model: opts.Model}
session/sidecar.go:269         StartSidecarWithOpts → prism sidecar --model <m> argv
sidecar/sidecar.go:139,143    cfg.AgentModel string — single value in SidecarCfg
sidecar/sidecar.go:1708-1721  uses cfg.AgentModel for UpsertStatusWithRootAgent call
harness/opencode/adapter.go:67-77   New(..., agentModel string) → a.agentModel field
harness/opencode/adapter.go:399-404 uses a.agentModel in DeliverInitialPrompt body
```

**The plumbing gap.** `agentModel` is a single string. To support per-role variation, one of two changes is needed:

**Option P-A: Replace `agentModel string` with `modelsByRole map[string]string`.** The adapter looks up the current role in the map. When a role has no entry, it falls back to a default. This makes `EffectiveModel(role)` authoritative from the map rather than from opencode's config file. Downstream call sites that use `agentModel` directly (`:399-404`) would be updated to look up by role.

**Option P-B: Leave `agentModel` as the default; add an additive `modelOverrides map[string]string`.** The adapter uses `modelsByRole[role]` when present, `agentModel` otherwise. Less invasive but adds a parallel field.

Option P-A is cleaner. The adapter's single-model field is already a soft gap (the interface method `EffectiveModel` already takes a role parameter and reads opencode's config rather than `a.agentModel` — it just doesn't consult a prism-managed map). Replacing the field unifies the two code paths.

**Review fan-out plumbing.** `review.Opts` would need a `ModelsByRole map[string]string` field (analogous to adding `--model-override` support to `prism review`). `internal/review/review.go` builds per-agent `session.SpawnOpts`; it would distribute role-specific entries from the map to the per-agent sidecar opts.

**Proxy-spawn path.** The host-API proxy request struct (`sidecar.go:3184`) currently passes `--model` as a single flag. It would need a `ModelOverrides map[string]string` field, serialised to a flag like `--model-override role=model` (repeated), to carry per-role overrides for proxy-spawned sessions. [uncertain — the proxy path shape is independently complex; see C.1 §2.1 caveat. This may be deferred to a separate implementation issue.]

### 3.4 Persistence surface

C.1 proposed a `spawn_inputs` table with a `model_variant_overrides` JSON column hook. That column is the natural home for per-role variation persistence:

```sql
CREATE TABLE spawn_inputs (
    instance_id     TEXT PRIMARY KEY,
    profile         TEXT,
    model           TEXT,
    variant         TEXT,
    model_variant_overrides  TEXT,  -- JSON: {"review-context": "google/gemini-2.5-pro"}
    ...
);
```

**What to persist.** The user-supplied per-role override map — not the fully resolved per-agent model IDs (those are already on `agent_status.model_id` / per-event). The override map records *intent*: "I asked for this specific role to use this specific model." The resolved values are derivable by combining the override map with the profile's role_mapping.

**Granularity.** For a five-agent review fan-out, each spawned agent session already has its own `instance_id`. The `spawn_inputs` row per-agent would carry the full override map (redundantly) unless a parent/group-level `spawn_inputs` row is introduced. A group-level row (keyed by `group_id` rather than `instance_id`) avoids the redundancy but adds complexity. [uncertain — depends on whether C.3's outcome capture design needs per-agent vs per-group `spawn_inputs` rows; defer to that discussion.]

---

## 4. CLI option shapes

### 4.1 Option A — Repeated `--model-override role=model` flags

```
prism spawn --profile anthropic --model-override review-context=google/gemini-2.5-pro ...
prism review <pr> --model-override review-context=google/gemini-2.5-pro
```

Multiple `--model-override` flags can be passed for multiple roles.

**Pros:**
- Shell-tab-completable in principle (known role names).
- Each flag is a self-contained `key=value` pair; easy to read in `ps` output.
- Natural extension of the existing `--model` flag; `--model` becomes the "apply to all roles" variant.
- Minimal parser complexity: `strings.Cut(val, "=")` gives role and model.

**Cons:**
- Verbose for more than two or three role overrides.
- `role=model` parsing is non-standard (not a cobra built-in type); requires a `StringToString` or slice+split approach.
- Ordering semantics if `--model` and `--model-override` are both passed must be explicitly defined (proposal: `--model-override` wins over `--model` for the named role; `--model` still applies to roles not overridden).

### 4.2 Option B — JSON/YAML inline override string

```
prism spawn --profile anthropic \
  --model-overrides '{"review-context":"google/gemini-2.5-pro","review-code":"anthropic/claude-opus-4-7"}' ...
```

**Pros:**
- One flag, arbitrary number of overrides.
- Lossless round-trip: the JSON string can be stored verbatim in the `spawn_inputs.model_variant_overrides` column.
- Consistent with other parts of the prism stack that use JSON blobs (e.g. `OPENCODE_CONFIG_CONTENT`).

**Cons:**
- Hostile shell ergonomics — requires quoting and escaping in interactive use.
- Not shell-completable.
- A typo in the JSON (missing comma, wrong brace) silently fails or produces a confusing error unless the parser is strict.
- Unusual for a CLI tool that otherwise uses individual `--flag value` conventions.

### 4.3 Option C — Profile-file extension (no new CLI flag)

Extend `profiles.json` with `agent_overrides` as described in §3.2. The user selects a named profile that already encodes the per-role variation:

```
prism spawn --profile anthropic-opus-context ...
```

No new CLI flags. The variation is expressed entirely in the Nix-managed profiles file.

**Pros:**
- Zero new CLI surface — no flag parsing, no shell escaping.
- The variation is versioned (profile names can be added/removed via Nix).
- Clean separation: profiles encode intent; spawn selects intent.
- Already consistent with how `--profile` works today.

**Cons:**
- Requires a Nix rebuild to add or change a profile — no ad-hoc experimentation.
- Doesn't serve the `prism review --model-override` ad-hoc use case (where the review is launched interactively with a one-off model override).
- Profile proliferation: every meaningful combination of role-model assignments becomes a named profile.
- Harder to use for automated A/B tests that dynamically pick model names.

### 4.4 Recommendation on CLI shape

Option A (repeated `--model-override role=model`) is the strongest candidate for ad-hoc and automated CLI use. It composes cleanly with existing flags, persists naturally as a map in `spawn_inputs`, and avoids shell-quoting hazards. Option C (profile-file extension) is the right complement for *named, stable* variations — users can author a profile rather than spelling out overrides every time. The two options are not mutually exclusive: implement A for ad-hoc use, and optionally extend profiles with `agent_overrides` (Option C) as a convenience wrapper.

Option B is not recommended as the primary surface due to ergonomics, but the internal representation (a JSON map) is appropriate for the persistence column.

---

## 5. Downstream call sites that assume a single model per spawn

The following file:line citations are sites that would need to be updated or considered when introducing per-role model variation:

| Site | Current assumption | Change needed |
|---|---|---|
| `internal/harness/opencode/adapter.go:49` | `agentModel string` field — single model per adapter | Replace with `modelsByRole map[string]string` (or add `modelOverrides map[string]string` additive field) |
| `internal/harness/opencode/adapter.go:67` | `New(..., agentModel string)` constructor signature | Update to accept a map; add a helper for the single-model case |
| `internal/harness/opencode/adapter.go:84` | `NewContainerMode(..., agentModel string)` constructor signature | Same as above |
| `internal/harness/opencode/adapter.go:75` | `agentModel: agentModel` assignment | Update to map assignment |
| `internal/harness/opencode/adapter.go:399-404` | `DeliverInitialPrompt` uses `a.agentModel` to set session model in body | Update to `a.modelForRole(a.agentRole)` |
| `internal/harness/opencode/adapter.go:640-665` | `EffectiveModel(role)` reads opencode.json, ignores prism's map | Update to consult prism's per-role map first, fall back to opencode.json |
| `internal/sidecar/sidecar.go:139,143` | `AgentModel string` in `SidecarCfg` | Extend to carry `ModelsByRole map[string]string` or a serialised form |
| `internal/sidecar/sidecar.go:1708-1721` | Uses `cfg.AgentModel` for `UpsertStatusWithRootAgent` | Update to use role-specific model from map |
| `internal/session/sidecar.go:211-254` | `StartSidecarOpts` has no per-role model map | Add `ModelsByRole map[string]string` field |
| `internal/session/spawn.go:623` | Constructs `StartSidecarOpts` with a single `Model` field (not shown above but inferred from `SpawnOpts`) | Add `ModelsByRole` propagation |
| `cmd/spawn.go:60,110,143-145` | `--model` single flag; `modelFlag` single string | Add `--model-override` slice flag; propagate map into `SpawnOpts` |
| `internal/sidecar/sidecar.go:3184-3214` | Proxy-spawn path passes `--model` single value | Add `ModelOverrides` to proxy request struct; serialise to repeated `--model-override` args |
| `internal/review/review.go:572-634` | `review.Opts` has no model map | Add `ModelsByRole map[string]string`; distribute per-agent entries in the review fan-out loop |

---

## 6. Interaction with `--variant` and `--profile`

### 6.1 `--profile`

A profile (e.g. `anthropic`) maps role buckets (primary, secondary, lightweight) to `{model, variant}`. The `--model` flag today overrides the primary-role agents' model within a selected profile (`BuildConfigContent:271-292`). The proposed `--model-override` flag should override at the named-agent level, taking precedence over the role-bucket assignment from the profile.

Precedence order (highest to lowest):
1. `--model-override role=model` for the named role
2. `--model` (overrides primary-role agents when `--profile` is set, or all agents otherwise)
3. Profile role-bucket assignment (`--profile` + role_mapping)
4. opencode's own agent config (harness default)

This is consistent with the existing `--model`-overrides-profile logic and extends it to the per-named-agent level.

### 6.2 `--variant`

`--variant` today applies a single variant string to *all* agents in `BuildConfigContent` (`profiles.go:296-304`). There is no per-role variant override at present.

If per-role model overrides are introduced, the natural extension is per-role variant overrides too (e.g. `--variant-override review-context=high`). However, this is not strictly necessary for the C.2 scope. The simpler first step is: `--model-override` specifies a model only; variant for that role is taken from the profile bucket or from `--variant` (global).

[uncertain — whether per-role variant overrides should be in scope for the same implementation PR as per-role model overrides, or filed as a follow-up. The main question is whether there is a concrete use case where the model and variant must be varied independently at the per-role level, vs. always together. If together, `--model-override review-context=google/gemini-2.5-pro/medium` (model/variant concatenated) may suffice; if independently, a second `--variant-override` flag is cleaner.]

### 6.3 Interaction summary

| Combination | Behaviour |
|---|---|
| `--profile P` | All role buckets resolved from P. |
| `--profile P --model M` | Primary-role agents use M; others from P. |
| `--profile P --model-override R=M` | Agent R uses M; others from P's role bucket. |
| `--profile P --model M --model-override R=M2` | Primary-role agents use M except R which uses M2; others from P. |
| `--model-override R=M` (no profile) | Agent R uses M; all other agents use their harness defaults. |
| `--variant V --model-override R=M` | Agent R uses M with variant V; other agents use harness defaults with variant V. |

---

## 7. Open questions

1. **Proxy-spawn path scope.** The host-API proxy-spawn handler does not carry `--variant` today (C.1 §2.1). Should per-role model overrides be in-scope for the proxy path in the same implementation issue, or deferred? [uncertain — depends on whether proxy-spawned sessions are used for A/B experiments in practice. If only CLI spawns are used for experimentation, deferring the proxy path is safe.]

2. **Per-agent vs. per-group `spawn_inputs` row for review fan-out.** When five review agents are spawned, each with the same per-role override map, should the map be stored once (on a group-level `spawn_inputs` row keyed by `group_id`) or five times (one per `instance_id`)? [uncertain — depends on C.3's outcome capture design; defer to that issue.]

3. **`--model-override` naming.** The flag name `--model-override` is proposed but `--role-model` is also plausible (matching the concept name more directly). [uncertain — naming bikeshed; not blocking for the proposal.]

4. **Container-mode per-agent config blobs vs. per-role model map.** In container mode, per-agent model is baked into the `OPENCODE_CONFIG_CONTENT` blob at spawn time (via `ContainerConfigForRole` or `BuildConfigContent`). For review agents, the blobs are pre-baked by Nix (`ContainerReviewGoalConfig` etc.). A CLI-level `--model-override review-context=X` would need to regenerate the per-agent blob at spawn time rather than using the pre-baked Nix blob. This is how `--model` already works (`BuildConfigContent` is called rather than `ContainerConfigForRole`), so the pattern exists — but it means pre-baked review-agent hardening (write/edit deny, task-tool disable) may be lost if the blob is regenerated from scratch. [uncertain — the hardening rules are in the Nix-generated blobs; a runtime `BuildConfigContent` call would not re-apply them unless the harness module logic is duplicated in Go. This is a meaningful trade-off that the implementation issue should address.]

5. **`EffectiveModel` authoritative source.** Currently `EffectiveModel` reads opencode's own config file on disk. Once prism carries a per-role map, it should return prism's value (which is what was used to configure opencode) rather than re-reading the file. The two should agree, but prism's value is the canonical input. [uncertain — whether the opencode.json fallback should be retained as a last-resort for manually-configured opencode sessions, or removed entirely to avoid confusion.]

---

## 8. Recommendation

**Adopt Option A (repeated `--model-override role=model`)** as the CLI surface for per-role model variation, complemented by an optional `agent_overrides` config-file extension for named profiles. The key design decisions in order of importance:

1. **CLI:** Add `--model-override role=model` (cobra `StringToStringVar` or manual slice+split); multiple flags allowed. Precedence: `--model-override` > `--model` > profile bucket.
2. **Config-file:** Extend `ProfileEntry` with optional `agent_overrides map[string]RoleConfig`; applied after role-bucket expansion in `BuildConfigContent`.
3. **Plumbing:** Replace `agentModel string` in the opencode adapter with `modelsByRole map[string]string`; update `EffectiveModel` to consult the prism-managed map first.
4. **Persistence:** Populate the `model_variant_overrides` JSON column in C.1's proposed `spawn_inputs` table with the user-supplied per-role map (serialised as `{"review-context": "google/gemini-2.5-pro"}`).

Open question 4 (container-mode hardening loss) is the most substantive risk and should be explicitly scoped in the implementation issue. All other questions are resolvable by the implementation author without additional design work.

This document contains no implementation; all code changes are left to the implementation issue to be filed post-synthesis (Track E).
