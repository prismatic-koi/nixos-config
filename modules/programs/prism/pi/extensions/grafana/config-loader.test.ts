// Tests for config-loader.ts. Runnable with `node --test config-loader.test.ts`
// or via pi's test runner. Uses node:test / node:assert only — no external
// test framework — matching the style of ../notion/slim.test.ts.

import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import {
  GrafanaConfigError,
  loadGrafanaBundle,
  parseDotenv,
} from "./config-loader.ts"

function tmpFile(contents: string): { path: string; cleanup: () => void } {
  const dir = mkdtempSync(join(tmpdir(), "grafana-mcp-test-"))
  const path = join(dir, "grafana-config")
  writeFileSync(path, contents)
  return { path, cleanup: () => rmSync(dir, { recursive: true, force: true }) }
}

test("parseDotenv: basic KEY=VALUE pairs", () => {
  const kv = parseDotenv("A=1\nB=two\n")
  assert.equal(kv["A"], "1")
  assert.equal(kv["B"], "two")
})

test("parseDotenv: blank lines and # comments are ignored", () => {
  const kv = parseDotenv("# a comment\n\nA=1\n#B=ignored\nB=real\n")
  assert.equal(kv["A"], "1")
  assert.equal(kv["B"], "real")
  assert.equal(Object.keys(kv).length, 2)
})

test("parseDotenv: preserves trailing whitespace and = in value", () => {
  const kv = parseDotenv("URL=https://example.com/a=b=c\n")
  assert.equal(kv["URL"], "https://example.com/a=b=c")
})

test("parseDotenv: strips trailing \\r (CRLF safety)", () => {
  const kv = parseDotenv("A=one\r\nB=two\r\n")
  assert.equal(kv["A"], "one")
  assert.equal(kv["B"], "two")
})

test("parseDotenv: duplicate key keeps last", () => {
  const kv = parseDotenv("A=first\nA=second\n")
  assert.equal(kv["A"], "second")
})

test("parseDotenv: throws on line without =", () => {
  assert.throws(() => parseDotenv("A=1\nnothing\n"), GrafanaConfigError)
})

test("parseDotenv: throws on empty key", () => {
  assert.throws(() => parseDotenv("=value\n"), GrafanaConfigError)
})

test("loadGrafanaBundle: happy path", () => {
  const { path, cleanup } = tmpFile(
    "GRAFANA_URL=https://grafana.example\nGRAFANA_SERVICE_ACCOUNT_TOKEN=abc123\n",
  )
  try {
    const b = loadGrafanaBundle(path)
    assert.equal(b.url, "https://grafana.example")
    assert.equal(b.apiKey, "abc123")
    assert.deepEqual(b.extraEnv, {})
  } finally {
    cleanup()
  }
})

test("loadGrafanaBundle: extra KEY=VALUE lines flow to extraEnv", () => {
  const { path, cleanup } = tmpFile(
    "GRAFANA_URL=https://x\nGRAFANA_SERVICE_ACCOUNT_TOKEN=k\nGRAFANA_ORG_ID=42\nCUSTOM=xyz\n",
  )
  try {
    const b = loadGrafanaBundle(path)
    assert.deepEqual(b.extraEnv, { GRAFANA_ORG_ID: "42", CUSTOM: "xyz" })
  } finally {
    cleanup()
  }
})

test("loadGrafanaBundle: missing URL throws GrafanaConfigError", () => {
  const { path, cleanup } = tmpFile("GRAFANA_SERVICE_ACCOUNT_TOKEN=k\n")
  try {
    assert.throws(() => loadGrafanaBundle(path), GrafanaConfigError)
  } finally {
    cleanup()
  }
})

test("loadGrafanaBundle: missing API key throws GrafanaConfigError", () => {
  const { path, cleanup } = tmpFile("GRAFANA_URL=https://x\n")
  try {
    assert.throws(() => loadGrafanaBundle(path), GrafanaConfigError)
  } finally {
    cleanup()
  }
})

test("loadGrafanaBundle: missing file throws GrafanaConfigError", () => {
  assert.throws(
    () => loadGrafanaBundle("/nonexistent/path/grafana-config"),
    GrafanaConfigError,
  )
})

test("loadGrafanaBundle: malformed file throws GrafanaConfigError", () => {
  const { path, cleanup } = tmpFile("this is not env-file syntax at all\n")
  try {
    assert.throws(() => loadGrafanaBundle(path), GrafanaConfigError)
  } finally {
    cleanup()
  }
})
