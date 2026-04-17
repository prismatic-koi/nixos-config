/**
 * Unit tests for prism-hooks.ts doom-loop detection logic.
 *
 * Run with: bun test prism-hooks.test.ts
 */
import { describe, expect, test } from "bun:test";
import { similarityKey } from "./prism-hooks";

// ── similarityKey tests ──────────────────────────────────────────────────────

describe("similarityKey — excluded tools", () => {
  test("read returns null", () => {
    expect(similarityKey("read", { filePath: "/foo.go" })).toBeNull();
  });

  test("grep returns null", () => {
    expect(similarityKey("grep", { pattern: "foo" })).toBeNull();
  });

  test("glob returns null", () => {
    expect(similarityKey("glob", { pattern: "**/*.ts" })).toBeNull();
  });

  test("todowrite returns null", () => {
    expect(similarityKey("todowrite", { todos: [] })).toBeNull();
  });
});

describe("similarityKey — bash similarity", () => {
  test("same command and positional produces the same key", () => {
    const a = similarityKey("bash", { command: "go test ./..." });
    const b = similarityKey("bash", { command: "go test ./..." });
    expect(a).toBe(b);
    expect(a).not.toBeNull();
  });

  test("git log -1 and git log -3 produce the same key (same command+first-positional)", () => {
    const a = similarityKey("bash", { command: "git log -1" });
    const b = similarityKey("bash", { command: "git log -3" });
    expect(a).toBe(b);
  });

  test("go test ./cmd/... and go build ./... produce different keys", () => {
    const a = similarityKey("bash", { command: "go test ./cmd/..." });
    const b = similarityKey("bash", { command: "go build ./..." });
    expect(a).not.toBe(b);
  });

  test("git status with different flags produces the same key", () => {
    const a = similarityKey("bash", { command: "git status" });
    const b = similarityKey("bash", { command: "git status --short" });
    // Both have same base "git" and first positional "status"
    expect(a).toBe(b);
  });

  test("nix build and nix run produce different keys (different first positional)", () => {
    const a = similarityKey("bash", { command: "nix build .#foo" });
    const b = similarityKey("bash", { command: "nix run .#bar" });
    expect(a).not.toBe(b);
  });

  test("key is prefixed with 'bash:'", () => {
    const key = similarityKey("bash", { command: "go test ./..." });
    expect(key).toMatch(/^bash:/);
  });
});

describe("similarityKey — edit similarity", () => {
  test("same file path produces the same key", () => {
    const a = similarityKey("edit", { filePath: "/workspace/foo.go", newString: "a" });
    const b = similarityKey("edit", { filePath: "/workspace/foo.go", newString: "b" });
    expect(a).toBe(b);
  });

  test("different file paths produce different keys", () => {
    const a = similarityKey("edit", { filePath: "/workspace/foo.go" });
    const b = similarityKey("edit", { filePath: "/workspace/bar.go" });
    expect(a).not.toBe(b);
  });

  test("key is prefixed with 'edit:'", () => {
    const key = similarityKey("edit", { filePath: "/workspace/foo.go" });
    expect(key).toMatch(/^edit:/);
  });
});

describe("similarityKey — write similarity", () => {
  test("same file path produces the same key regardless of content", () => {
    const a = similarityKey("write", { filePath: "/workspace/foo.go", content: "package foo" });
    const b = similarityKey("write", { filePath: "/workspace/foo.go", content: "package bar" });
    expect(a).toBe(b);
  });

  test("different file paths produce different keys", () => {
    const a = similarityKey("write", { filePath: "/workspace/foo.go" });
    const b = similarityKey("write", { filePath: "/workspace/bar.go" });
    expect(a).not.toBe(b);
  });

  test("key is prefixed with 'write:'", () => {
    const key = similarityKey("write", { filePath: "/workspace/foo.go" });
    expect(key).toMatch(/^write:/);
  });
});

describe("similarityKey — webfetch similarity", () => {
  test("same URL produces the same key", () => {
    const a = similarityKey("webfetch", { url: "https://example.com" });
    const b = similarityKey("webfetch", { url: "https://example.com" });
    expect(a).toBe(b);
  });

  test("different URLs produce different keys", () => {
    const a = similarityKey("webfetch", { url: "https://example.com/foo" });
    const b = similarityKey("webfetch", { url: "https://example.com/bar" });
    expect(a).not.toBe(b);
  });

  test("key is prefixed with 'webfetch:'", () => {
    const key = similarityKey("webfetch", { url: "https://example.com" });
    expect(key).toMatch(/^webfetch:/);
  });
});

describe("similarityKey — default (byte-exact)", () => {
  test("same args produce the same key for unknown tools", () => {
    const a = similarityKey("task", { subagent_type: "explore", prompt: "search code" });
    const b = similarityKey("task", { subagent_type: "explore", prompt: "search code" });
    expect(a).toBe(b);
  });

  test("different args produce different keys for unknown tools", () => {
    const a = similarityKey("task", { subagent_type: "explore", prompt: "search code" });
    const b = similarityKey("task", { subagent_type: "review", prompt: "search code" });
    expect(a).not.toBe(b);
  });

  test("key is prefixed with the tool name", () => {
    const key = similarityKey("task", { prompt: "foo" });
    expect(key).toMatch(/^task:/);
  });
});

// ── Doom-loop state machine simulation ───────────────────────────────────────
//
// These tests simulate the detector state machine without running the full
// plugin factory, verifying: suppression after firing, pattern-break reset,
// threshold counting, and session isolation.

// Inline minimal state machine (mirrors prism-hooks.ts detector logic).
interface DoomLoopState {
  currentKey: string | null;
  consecutiveCount: number;
  fired: boolean;
}

const DOOM_LOOP_THRESHOLD = 5;

const EXCLUDED = new Set(["read", "grep", "glob", "todowrite"]);

function processToolCall(
  state: DoomLoopState,
  tool: string,
  args: any
): { fired: boolean; suppressed: boolean } {
  if (EXCLUDED.has(tool)) {
    state.currentKey = null;
    state.consecutiveCount = 0;
    state.fired = false;
    return { fired: false, suppressed: false };
  }

  const key = similarityKey(tool, args);
  if (key === null) {
    return { fired: false, suppressed: false };
  }

  if (key === state.currentKey) {
    if (!state.fired) {
      state.consecutiveCount++;
      if (state.consecutiveCount >= DOOM_LOOP_THRESHOLD) {
        state.fired = true;
        return { fired: true, suppressed: false };
      }
    } else {
      // Suppressed.
      return { fired: false, suppressed: true };
    }
  } else {
    state.currentKey = key;
    state.consecutiveCount = 1;
    state.fired = false;
  }
  return { fired: false, suppressed: false };
}

function newState(): DoomLoopState {
  return { currentKey: null, consecutiveCount: 0, fired: false };
}

describe("doom-loop state machine", () => {
  test("fires on the 5th consecutive bash call with same key", () => {
    const state = newState();
    const calls = Array(5).fill(null).map(() =>
      processToolCall(state, "bash", { command: "go test ./..." })
    );
    expect(calls[4].fired).toBe(true);
    expect(calls.slice(0, 4).every((c) => !c.fired)).toBe(true);
  });

  test("does NOT fire on 4 consecutive calls", () => {
    const state = newState();
    const calls = Array(4).fill(null).map(() =>
      processToolCall(state, "bash", { command: "go test ./..." })
    );
    expect(calls.every((c) => !c.fired)).toBe(true);
  });

  test("after firing, 6th/7th+ calls are suppressed (no additional fire)", () => {
    const state = newState();
    // Fire on call 5.
    for (let i = 0; i < 5; i++) {
      processToolCall(state, "bash", { command: "go test ./..." });
    }
    const call6 = processToolCall(state, "bash", { command: "go test ./..." });
    const call7 = processToolCall(state, "bash", { command: "go test ./..." });
    expect(call6.fired).toBe(false);
    expect(call6.suppressed).toBe(true);
    expect(call7.fired).toBe(false);
    expect(call7.suppressed).toBe(true);
  });

  test("pattern break (different bash command) resets suppression", () => {
    const state = newState();
    // Fire.
    for (let i = 0; i < 5; i++) {
      processToolCall(state, "bash", { command: "go test ./..." });
    }
    expect(state.fired).toBe(true);
    // Different pattern breaks the run.
    processToolCall(state, "bash", { command: "go build ./..." });
    expect(state.fired).toBe(false);
    expect(state.consecutiveCount).toBe(1);
  });

  test("new 5-call loop after pattern break fires a new event", () => {
    const state = newState();
    // First loop: fires on call 5.
    for (let i = 0; i < 5; i++) {
      processToolCall(state, "bash", { command: "go test ./..." });
    }
    // Break: resets state; "go build" is now count=1.
    processToolCall(state, "bash", { command: "go build ./..." });
    // 4 more calls needed to reach the threshold (total consecutive = 5).
    const calls = Array(4).fill(null).map(() =>
      processToolCall(state, "bash", { command: "go build ./..." })
    );
    // The 5th consecutive "go build" call (4 loop calls after the break call) fires.
    expect(calls[3].fired).toBe(true);
    expect(calls.slice(0, 3).every((c) => !c.fired)).toBe(true);
  });

  test("excluded tool (read) resets state without firing", () => {
    const state = newState();
    for (let i = 0; i < 4; i++) {
      processToolCall(state, "bash", { command: "go test ./..." });
    }
    expect(state.consecutiveCount).toBe(4);
    // Read breaks the run.
    processToolCall(state, "read", { filePath: "/foo.go" });
    expect(state.currentKey).toBeNull();
    expect(state.consecutiveCount).toBe(0);
    expect(state.fired).toBe(false);
  });

  test("excluded tool (todowrite) resets state without firing", () => {
    const state = newState();
    for (let i = 0; i < 4; i++) {
      processToolCall(state, "bash", { command: "go test ./..." });
    }
    processToolCall(state, "todowrite", { todos: [] });
    expect(state.currentKey).toBeNull();
    expect(state.fired).toBe(false);
  });

  test("session isolation — two states do not cross-contaminate", () => {
    const stateA = newState();
    const stateB = newState();
    // Session A: 4 calls.
    for (let i = 0; i < 4; i++) {
      processToolCall(stateA, "bash", { command: "go test ./..." });
    }
    // Session B: 4 calls.
    for (let i = 0; i < 4; i++) {
      processToolCall(stateB, "bash", { command: "go test ./..." });
    }
    // Neither has fired yet.
    expect(stateA.fired).toBe(false);
    expect(stateB.fired).toBe(false);
    // 5th call on A — only A fires.
    const resA = processToolCall(stateA, "bash", { command: "go test ./..." });
    expect(resA.fired).toBe(true);
    expect(stateB.fired).toBe(false);
  });

  test("edit/write same path fires on 5th call", () => {
    const state = newState();
    const calls = Array(5).fill(null).map(() =>
      processToolCall(state, "edit", { filePath: "/workspace/foo.go", newString: "x" })
    );
    expect(calls[4].fired).toBe(true);
  });

  test("edit/write different paths do not fire", () => {
    const state = newState();
    const paths = ["/a.go", "/b.go", "/c.go", "/d.go", "/e.go"];
    const calls = paths.map((p) =>
      processToolCall(state, "edit", { filePath: p })
    );
    expect(calls.every((c) => !c.fired)).toBe(true);
  });

  test("webfetch same URL fires on 5th call", () => {
    const state = newState();
    const calls = Array(5).fill(null).map(() =>
      processToolCall(state, "webfetch", { url: "https://example.com" })
    );
    expect(calls[4].fired).toBe(true);
  });

  test("webfetch different URLs do not fire", () => {
    const state = newState();
    const urls = ["https://a.com", "https://b.com", "https://c.com", "https://d.com", "https://e.com"];
    const calls = urls.map((u) =>
      processToolCall(state, "webfetch", { url: u })
    );
    expect(calls.every((c) => !c.fired)).toBe(true);
  });
});
