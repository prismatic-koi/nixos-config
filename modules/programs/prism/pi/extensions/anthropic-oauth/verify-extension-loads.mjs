#!/usr/bin/env node
// Regression guard for issue #2428 / PR #2429 — the anthropic-oauth extension
// has broken across a pi-coding-agent minor bump before (0.80.7 -> 0.80.8
// removed `AuthStorage` from the barrel and dropped the
// `ModelRegistry.create()` static factory), and only failed at *interactive
// pi launch*, not at `pi --version` or `nix build`.
//
// This script loads the extension end-to-end against a real, installed
// pi-coding-agent build using pi's own extension-loading mechanism (jiti,
// aliased the same way pi's `dist/core/extensions/loader.js` aliases it for
// Node/dev mode), invokes its async default export with a fake ExtensionAPI,
// and asserts the anthropic provider actually registers with a non-empty
// model list and all four OAuth handlers wired.
//
// Run after any pi-coding-agent version bump (overlay pin or nixpkgs bump)
// to catch this regression class before it reaches an interactive `pi`
// session:
//
//   node modules/programs/prism/pi/extensions/anthropic-oauth/verify-extension-loads.mjs
//
// By default this resolves the installed pi-coding-agent via `command -v pi`
// (the wrapped binary at $out/bin/pi, sibling to $out/lib/node_modules/...).
// Override with PI_INSTALL_ROOT=/nix/store/...-pi-coding-agent-X.Y.Z to point
// at a specific build (e.g. one built via the overlay but not yet the
// default `pkgs.pi-coding-agent`).

import { execFileSync } from "node:child_process"
import { existsSync, realpathSync } from "node:fs"
import path from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"

const extensionPath = fileURLToPath(new URL("./index.ts", import.meta.url))

function resolveInstallRoot() {
  if (process.env.PI_INSTALL_ROOT) {
    return process.env.PI_INSTALL_ROOT
  }
  const piBin = execFileSync("/bin/sh", ["-c", "command -v pi"]).toString().trim()
  if (!piBin) {
    throw new Error(
      "could not locate `pi` on PATH; set PI_INSTALL_ROOT to the pi-coding-agent $out store path",
    )
  }
  const resolvedBin = realpathSync(piBin)
  // $out/bin/pi -> $out
  return path.dirname(path.dirname(resolvedBin))
}

async function main() {
  const installRoot = resolveInstallRoot()
  const pkgRoot = path.join(installRoot, "lib/node_modules/pi-monorepo")
  const nm = path.join(pkgRoot, "node_modules/@earendil-works")

  for (const p of [pkgRoot, nm]) {
    if (!existsSync(p)) {
      throw new Error(
        `expected pi-coding-agent install layout not found at ${p} (installRoot=${installRoot}); ` +
          "is PI_INSTALL_ROOT pointing at a buildNpmPackage-based pi-coding-agent $out?",
      )
    }
  }

  const jitiEntry = path.join(pkgRoot, "node_modules/jiti/lib/jiti.mjs")
  const { createJiti } = await import(pathToFileURL(jitiEntry).href)

  const jiti = createJiti(import.meta.url, {
    moduleCache: false,
    // Mirrors dist/core/extensions/loader.js::getAliases() for Node/dev
    // mode (see that file for the Bun-binary virtualModules equivalent).
    alias: {
      "@earendil-works/pi-coding-agent": path.join(pkgRoot, "dist/index.js"),
      "@earendil-works/pi-agent-core": path.join(nm, "pi-agent-core/dist/index.js"),
      "@earendil-works/pi-tui": path.join(nm, "pi-tui/dist/index.js"),
      "@earendil-works/pi-ai/providers/all": path.join(nm, "pi-ai/dist/providers/all.js"),
      "@earendil-works/pi-ai/compat": path.join(nm, "pi-ai/dist/compat.js"),
      "@earendil-works/pi-ai/oauth": path.join(nm, "pi-ai/dist/oauth.js"),
      "@earendil-works/pi-ai": path.join(nm, "pi-ai/dist/compat.js"),
    },
  })

  const factory = await jiti.import(extensionPath, { default: true })
  if (typeof factory !== "function") {
    throw new Error(
      `expected ${extensionPath} default export to be a function (pi extension factory), got ${typeof factory}`,
    )
  }

  const registered = []
  const fakePi = {
    registerProvider(nameOrProvider, config) {
      registered.push({ nameOrProvider, config })
    },
    events: { on() {}, off() {}, emit() {} },
  }

  await factory(fakePi)

  if (registered.length !== 1) {
    throw new Error(`expected exactly one pi.registerProvider() call, got ${registered.length}`)
  }
  const [{ nameOrProvider, config }] = registered
  if (nameOrProvider !== "anthropic") {
    throw new Error(`expected provider name "anthropic", got ${JSON.stringify(nameOrProvider)}`)
  }
  const models = config?.models ?? []
  if (models.length === 0) {
    throw new Error(
      "anthropic provider registered with an EMPTY model list — registry.refresh() likely " +
        "not awaited, or the registry API shape changed upstream (see issue #2428)",
    )
  }
  const oauthHandlers = ["login", "refreshToken", "getApiKey"]
  const missingHandlers = oauthHandlers.filter((h) => typeof config?.oauth?.[h] !== "function")
  if (missingHandlers.length > 0) {
    throw new Error(`missing OAuth handler(s): ${missingHandlers.join(", ")}`)
  }

  console.log(
    `OK: anthropic-oauth extension loaded against ${path.basename(installRoot)}; ` +
      `registered "anthropic" with ${models.length} model(s), OAuth handlers present.`,
  )
}

main().catch((err) => {
  console.error("FAILED:", err.message ?? err)
  process.exitCode = 1
})
