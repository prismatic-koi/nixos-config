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
import { existsSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs"
import os from "node:os"
import path from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"

const extensionPath = fileURLToPath(new URL("./index.ts", import.meta.url))

// Declared by `piModels` in modules/programs/prism/pi.nix, not by pi itself.
const MODELS_JSON_MODEL_ID = "claude-fable-5-1"

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

  // Assertions below are shared by both load passes.
  function assertRegistration(registered, label) {
    if (registered.length !== 1) {
      throw new Error(
        `${label}: expected exactly one pi.registerProvider() call, got ${registered.length}`,
      )
    }
    const [{ nameOrProvider, config }] = registered
    if (nameOrProvider !== "anthropic") {
      throw new Error(
        `${label}: expected provider name "anthropic", got ${JSON.stringify(nameOrProvider)}`,
      )
    }
    const models = config?.models ?? []
    if (models.length === 0) {
      throw new Error(
        `${label}: anthropic provider registered with an EMPTY model list — registry.refresh() likely ` +
          "not awaited, or the registry API shape changed upstream (see issue #2428)",
      )
    }
    // Every layer that adds a model upserts by `id`. A repeat means one of
    // them appended instead, giving a duplicate row in the model picker.
    const seen = new Set()
    const duplicates = models.map((m) => m.id).filter((id) => seen.size === seen.add(id).size)
    if (duplicates.length > 0) {
      throw new Error(
        `${label}: duplicate model id(s) registered: ${[...new Set(duplicates)].join(", ")}`,
      )
    }
    return { config, models }
  }

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

  async function load() {
    const registered = []
    const fakePi = {
      registerProvider(nameOrProvider, config) {
        registered.push({ nameOrProvider, config })
      },
      events: { on() {}, off() {}, emit() {} },
    }
    await factory(fakePi)
    return registered
  }

  // ── Pass 1: the installed catalogue as-is (issue #2428 guard) ────────────
  const { config, models } = assertRegistration(await load(), "bundled catalogue")

  const oauthHandlers = ["login", "refreshToken", "getApiKey"]
  const missingHandlers = oauthHandlers.filter((h) => typeof config?.oauth?.[h] !== "function")
  if (missingHandlers.length > 0) {
    throw new Error(`missing OAuth handler(s): ${missingHandlers.join(", ")}`)
  }

  // ── Pass 2: a models.json-only model (issue #2918) ───────────────────────
  //
  // Each sub-pass gets a throwaway agent dir, so the real ~/.pi/agent cannot
  // satisfy any of them. The fixtures carry only the fields the assertions
  // read.
  const modelsJsonFixture = {
    providers: {
      anthropic: {
        models: [
          {
            id: MODELS_JSON_MODEL_ID,
            name: "Claude Fable 5.1",
            api: "anthropic-messages",
            baseUrl: "https://api.anthropic.com",
            reasoning: true,
            thinkingLevelMap: { off: null, xhigh: "xhigh", max: "max" },
            contextWindow: 1000000,
            maxTokens: 128000,
            compat: { forceAdaptiveThinking: true },
          },
        ],
      },
    },
  }

  // A refreshed catalogue cache, as `pi update models` writes it. It feeds the
  // same base list as the bundled catalogue, so models.json must upsert over
  // it rather than append a second copy.
  //
  // `lastModified` is load-bearing: remote-catalog-provider.js drops any
  // stored entry whose value is absent or older than the bundled manifest's
  // generation time, so a fixture without it never reaches the upsert and 2c
  // silently becomes a copy of 2b.
  const modelsStoreFixture = {
    anthropic: {
      checkedAt: Date.now(),
      lastModified: Date.now(),
      models: [
        {
          id: MODELS_JSON_MODEL_ID,
          name: "Claude Fable 5.1",
          api: "anthropic-messages",
          provider: "anthropic",
          baseUrl: "https://api.anthropic.com",
          reasoning: true,
          input: ["text", "image"],
          cost: { input: 10, output: 50, cacheRead: 0.25, cacheWrite: 12.5 },
          contextWindow: 1000000,
          maxTokens: 128000,
          thinkingLevelMap: { off: null, xhigh: "xhigh", max: "max" },
          compat: { forceAdaptiveThinking: true, supportsStrictTools: true },
        },
      ],
    },
  }

  async function loadWithAgentDir(label, files) {
    const dir = mkdtempSync(path.join(os.tmpdir(), "pi-agent-verify-"))
    const saved = process.env.PI_CODING_AGENT_DIR
    try {
      for (const [name, content] of Object.entries(files)) {
        writeFileSync(path.join(dir, name), JSON.stringify(content))
      }
      process.env.PI_CODING_AGENT_DIR = dir
      const { models } = assertRegistration(await load(), label)
      return models.filter((m) => m.id === MODELS_JSON_MODEL_ID)
    } finally {
      if (saved === undefined) delete process.env.PI_CODING_AGENT_DIR
      else process.env.PI_CODING_AGENT_DIR = saved
      rmSync(dir, { recursive: true, force: true })
    }
  }

  // 2a — the control. The model must be absent without the file, or 2b proves
  // nothing. A failure here means pi now bundles it, and pi.nix may not need
  // to declare it.
  const control = await loadWithAgentDir("empty agent dir", {})
  if (control.length !== 0) {
    throw new Error(
      `${MODELS_JSON_MODEL_ID} is now in the bundled catalogue (${control.length} entr(y|ies)); ` +
        "the models.json layering check below no longer proves anything — see issue #2918",
    )
  }

  // 2b — models.json alone puts the model in the list, exactly once.
  const layered = await loadWithAgentDir("models.json layer", {
    "models.json": modelsJsonFixture,
  })
  if (layered.length !== 1) {
    throw new Error(
      `expected models.json model ${MODELS_JSON_MODEL_ID} to register exactly once, got ${layered.length}`,
    )
  }
  const fromModelsJson = layered[0]

  // 2c-control — the store fixture alone must land, or 2c below is just 2b
  // again. This is what catches a future pi that changes the drop rule.
  const storeOnly = await loadWithAgentDir("models-store only", {
    "models-store.json": modelsStoreFixture,
  })
  if (storeOnly.length !== 1) {
    throw new Error(
      `expected the models-store fixture alone to register ${MODELS_JSON_MODEL_ID} once, got ` +
        `${storeOnly.length} — pi discarded the fixture, so the upsert check below is vacuous`,
    )
  }

  // 2c — the id in pi.nix must match the catalogue's, or the upsert appends.
  const deduped = await loadWithAgentDir("models.json over models-store", {
    "models.json": modelsJsonFixture,
    "models-store.json": modelsStoreFixture,
  })
  if (deduped.length !== 1) {
    throw new Error(
      `expected ${MODELS_JSON_MODEL_ID} to register exactly once when a refreshed catalogue ` +
        `already carries it, got ${deduped.length}`,
    )
  }

  // thinkingLevelMap and the compat flag must survive registration, and
  // per-model headers must not — buildOAuthHeaders owns the OAuth headers.
  if (fromModelsJson.thinkingLevelMap?.xhigh !== "xhigh") {
    throw new Error(
      `expected ${MODELS_JSON_MODEL_ID} thinkingLevelMap.xhigh to survive registration, got ` +
        JSON.stringify(fromModelsJson.thinkingLevelMap),
    )
  }
  if (fromModelsJson.compat?.forceAdaptiveThinking !== true) {
    throw new Error(
      `expected ${MODELS_JSON_MODEL_ID} compat.forceAdaptiveThinking to survive registration, got ` +
        JSON.stringify(fromModelsJson.compat),
    )
  }
  if (fromModelsJson.headers !== undefined) {
    throw new Error(
      `${MODELS_JSON_MODEL_ID} registered with per-model headers; the OAuth path builds its own ` +
        `(got ${JSON.stringify(fromModelsJson.headers)})`,
    )
  }

  console.log(
    `OK: anthropic-oauth extension loaded against ${path.basename(installRoot)}; ` +
      `registered "anthropic" with ${models.length} model(s), OAuth handlers present; ` +
      `models.json-only model ${MODELS_JSON_MODEL_ID} registered exactly once, ` +
      "with and without a refreshed catalogue entry.",
  )
}

main().catch((err) => {
  console.error("FAILED:", err.message ?? err)
  process.exitCode = 1
})
