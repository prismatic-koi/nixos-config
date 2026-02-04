# Testing Configuration Changes

## Core Principle: Verify No Functional Changes

When making changes that should only affect specific platforms or configurations, the **core principle** is to verify that other configurations have no **functional changes** - meaning the actual output behavior remains identical, even if the derivation path changes due to structural refactoring.

**Two types of testing:**
1. **Derivation path comparison** - Quick signal that something changed (or didn't)
2. **Functional output comparison** - Verifies the actual configuration output is identical

Both are valuable, but **functional verification is the authoritative test** when derivation paths differ.

### Example: Testing that NixOS config is unchanged

```bash
# Get derivation path before changes
git checkout HEAD~1
nix eval .#nixosConfigurations.navi.config.system.build.toplevel.drvPath 2>&1 | grep '^"' > /tmp/navi-before.txt

# Get derivation path after changes
git checkout -
nix eval .#nixosConfigurations.navi.config.system.build.toplevel.drvPath 2>&1 | grep '^"' > /tmp/navi-after.txt

# Compare
echo "Before: $(cat /tmp/navi-before.txt)"
echo "After:  $(cat /tmp/navi-after.txt)"
diff /tmp/navi-before.txt /tmp/navi-after.txt && echo "✓ No changes!" || echo "✗ Changed"
```

### One-liner version:

```bash
git checkout HEAD~1 && \
  nix eval .#nixosConfigurations.navi.config.system.build.toplevel.drvPath 2>&1 | grep '^"' > /tmp/navi-before.txt && \
  git checkout - && \
  nix eval .#nixosConfigurations.navi.config.system.build.toplevel.drvPath 2>&1 | grep '^"' > /tmp/navi-after.txt && \
  echo "Before: $(cat /tmp/navi-before.txt)" && \
  echo "After:  $(cat /tmp/navi-after.txt)" && \
  diff /tmp/navi-before.txt /tmp/navi-after.txt && echo "✓ No changes detected!" || echo "✗ Derivations differ"
```

### Testing other configurations

Replace `navi` with any configuration name:

```bash
# For Darwin m1mac
nix eval .#darwinConfigurations.m1mac.config.system.build.toplevel.drvPath

# For NixOS tui
nix eval .#nixosConfigurations.tui.config.system.build.toplevel.drvPath
```

### Why this works

Nix is deterministic - if the configuration produces the same output, it will have the same derivation path. If the derivation paths differ, something in the configuration changed (even if it still builds successfully).

This is particularly useful when:
- Refactoring code structure without changing functionality
- Adding platform guards that should only affect one platform
- Ensuring changes to shared modules don't break specific configurations

## Understanding Derivation Changes

### When Derivation Changes Are Expected

**Structural refactoring** will change derivations even if the final output is functionally identical:

- **Using `lib.mkMerge`**: Restructuring with `lib.mkMerge` changes how NixOS evaluates attributes, which changes derivation hashes
- **Reordering options**: Moving options around or splitting into conditional blocks
- **Module reorganization**: Breaking a module into platform-specific sections

**Example:** Converting a simple module to use `lib.mkMerge` with `lib.mkIf isLinux` guards will change `/etc` and `activate` script derivations because the evaluation path changed, even though the final Linux config is identical.

### When Derivation Changes Are Unexpected

Derivations should NOT change if:
- Adding code that only runs on other platforms (with proper guards)
- Adding comments or documentation
- Renaming internal variables that don't affect outputs

### Deep Comparison of Derivations

If you need to understand **what** changed in a derivation:

```bash
# 1. Save derivations from before/after (removes narHash which changes on rebuild)
git checkout HEAD~1
nix derivation show .#nixosConfigurations.navi.config.system.build.toplevel 2>/dev/null | \
  jq 'del(..|.narHash?)' > /tmp/navi-before.json

git checkout -
nix derivation show .#nixosConfigurations.navi.config.system.build.toplevel 2>/dev/null | \
  jq 'del(..|.narHash?)' > /tmp/navi-after.json

# 2. Compare without store paths (to see structural changes)
diff -u \
  <(jq -S . /tmp/navi-before.json | grep -v '"/nix/store/') \
  <(jq -S . /tmp/navi-after.json | grep -v '"/nix/store/') | less

# 3. Find which store paths changed
diff <(jq -r '.. | strings | select(startswith("/nix/store/"))' /tmp/navi-before.json | sort -u) \
     <(jq -r '.. | strings | select(startswith("/nix/store/"))' /tmp/navi-after.json | sort -u)
```

Common changes to look for:
- **`buildCommand` differences**: Shows what scripts changed
- **`/etc` derivation changed**: Home-manager or system config changed
- **`activate` script changed**: Usually follows from `/etc` changes
- **Output order changed**: Cosmetic, not functional

### Accepting Structural Changes

For **structural refactoring** (like migrating to cross-platform), derivation changes are acceptable if:
1. The changes are intentional (module restructuring)
2. The build succeeds on all platforms
3. Functional testing confirms behavior is unchanged

In these cases, the derivation comparison serves as a **signal** that something changed, but doesn't indicate a problem.

## Functional Testing: Verifying Actual Output

When derivation paths differ but you need to verify there are no functional changes, test the actual configuration output that matters.

### Why Functional Testing?

Derivation paths change with structural refactoring (using `lib.mkMerge`, reordering options, etc.), but this doesn't mean the actual configuration changed. **Functional testing verifies what the system will actually do**, which is the real concern.

### Testing Strategy by Module Type

Different modules require different testing approaches:

#### Application Configuration Files

For programs that generate config files (kitty, tmux, neovim, etc.), compare the actual config output:

```bash
# Example: Testing kitty configuration
git checkout HEAD~1
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.kitty.extraConfig 2>&1 | grep -v "^warning" > /tmp/kitty-before.txt

git checkout -
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.kitty.extraConfig 2>&1 | grep -v "^warning" > /tmp/kitty-after.txt

echo "=== DIFF ==="
diff /tmp/kitty-before.txt /tmp/kitty-after.txt && echo "✓ No functional changes!" || echo "✗ Config differs"
```

**What to test:**
- `programs.<app>.extraConfig` - The actual config file content
- `programs.<app>.settings` - Settings that get written to config
- `programs.<app>.enable` - Whether the program is enabled

#### Package Lists

For package installations, compare the installed package list:

```bash
# Compare system packages
nix eval .#nixosConfigurations.navi.config.environment.systemPackages --apply 'map (p: p.name)' --json

# Compare home-manager packages
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.home.packages --apply 'map (p: p.name)' --json
```

#### Service Configurations

For services, check that services are enabled/disabled correctly:

```bash
# System services
nix eval .#nixosConfigurations.navi.config.systemd.services --apply builtins.attrNames --json

# User services
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.systemd.user.services --apply builtins.attrNames --json
```

#### Environment Variables

For shell configurations:

```bash
# Check environment variables
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.home.sessionVariables --json

# Check shell aliases
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.zsh.shellAliases --json
```

### Comprehensive Functional Testing Workflow

When making cross-platform or structural changes:

```bash
# 1. Test derivation paths (quick signal)
git checkout HEAD~1 && \
  nix eval .#nixosConfigurations.navi.config.system.build.toplevel.drvPath 2>&1 | grep '^"' > /tmp/navi-before.txt && \
  git checkout - && \
  nix eval .#nixosConfigurations.navi.config.system.build.toplevel.drvPath 2>&1 | grep '^"' > /tmp/navi-after.txt && \
  diff /tmp/navi-before.txt /tmp/navi-after.txt && echo "✓ Derivations match!" || echo "⚠ Derivations differ - proceed to functional testing"

# 2. If derivations differ, test specific module outputs
# Example for a program module change:
git checkout HEAD~1
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.PROGRAM.extraConfig 2>&1 | grep -v "^warning" > /tmp/program-before.txt

git checkout -
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.PROGRAM.extraConfig 2>&1 | grep -v "^warning" > /tmp/program-after.txt

diff /tmp/program-before.txt /tmp/program-after.txt && echo "✓ Config unchanged!" || echo "✗ Config changed - review differences"

# 3. Test other related settings
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.PROGRAM.font.name 2>&1 | grep -v "^warning"
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.PROGRAM.font.size 2>&1 | grep -v "^warning"
```

### Real-World Example: Kitty Cross-Platform Migration

When migrating kitty module to support cross-platform with `lib.mkMerge`:

**Step 1: Check derivations** (they differed - `/etc` and `activate` changed)

**Step 2: Verify functional output:**
```bash
# Compare actual kitty config
git checkout HEAD~1
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.kitty.extraConfig 2>&1 | head -50 > /tmp/kitty-before.txt

git checkout -
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.kitty.extraConfig 2>&1 | head -50 > /tmp/kitty-after.txt

diff /tmp/kitty-before.txt /tmp/kitty-after.txt
# Result: ✓ Identical!

# Verify font settings (platform-specific)
nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.kitty.font.name 2>&1 | grep -v "^warning"
# Result: "JetBrainsMono Nerd Font" (correct for Linux)

nix eval .#nixosConfigurations.navi.config.home-manager.users.ben.programs.kitty.font.size 2>&1 | grep -v "^warning"
# Result: 12 (correct for Linux)
```

**Outcome:** Derivation paths differed due to `lib.mkMerge` restructuring, but all functional outputs were identical. ✅ Safe to proceed.

### When to Use Each Testing Method

| Scenario | Primary Test | Secondary Test |
|----------|-------------|----------------|
| Simple refactoring (no restructuring) | Derivation path | N/A |
| Structural refactoring with `lib.mkMerge` | Functional output | Derivation (informational) |
| Adding platform-specific code | Both (should match) | N/A |
| Adding new options/features | Functional output | Verify new behavior |
| Bug fixes | Functional output | Verify fix works |

### Key Principle

**When derivation paths differ but functional outputs are identical, the change is structurally different but functionally equivalent** - this is acceptable for refactoring work, especially when migrating to cross-platform support.

The goal is not to prevent all derivation changes, but to ensure **no unintended functional changes** affect existing configurations.
