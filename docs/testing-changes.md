# Testing Configuration Changes

## Verifying No Unintended Changes

When making changes that should only affect specific platforms or configurations, you can verify that other configurations remain unchanged by comparing derivation paths before and after.

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
