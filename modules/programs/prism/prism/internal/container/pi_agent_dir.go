package container

// pi_agent_dir.go — what a sandboxed pi session can write in the pi agent
// dir (~/.pi/agent), for bwrap and for sandbox-exec (#3284).
//
// A worker is not trusted. Every later session reads the agent dir: host
// sessions, coordinators, and review agents. pi loads code and prompts from
// it with no project-trust check: settings.json (the extensions list),
// extensions/, skills/, SYSTEM.md, APPEND_SYSTEM.md, mcp.json (stdio MCP
// server commands), models.json, and prompts/. pi also puts bin/ first on
// the PATH of each bash tool call. So a sandboxed session must not create,
// change, or replace any path in the agent dir, except the paths below.
//
// pi 1.4 must write these paths during a normal session:
//
//   - auth.json: OAuth token refresh, a write in place under a lock.
//   - auth.json.lock, settings.json.lock, trust.json.lock, and
//     models-store.json.lock: proper-lockfile lock dirs. pi takes the lock
//     on each read of the file, also at start. The lock dir is a sibling of
//     the file, so pi must make dirs in the root of the agent dir. If it
//     cannot, pi exits at start.
//   - sessions/<encoded-cwd>/: the session files of the session.
//
// The extensions write these paths:
//
//   - atlassian-mcp-oauth.json: Atlassian MCP OAuth refresh, a write in
//     place.
//   - notion-mcp-oauth.json and notion-mcp-client.json: Notion MCP OAuth
//     refresh, a temp file .notion-mcp.tmp.* that is renamed onto the
//     file, under the lock dir notion-mcp-oauth.json.lock.
//
// bwrap cannot give "create only these names" in a shared dir. The agent
// dir in a bwrap sandbox is therefore a private dir: bwrap makes it in its
// own tmpfs root, and it goes away with the sandbox. prism binds each host
// entry into it read-only, binds the credential files read-write, and binds
// the sessions dir of the worktree read-write. The lock dirs are private to
// the sandbox. The Notion token store is not bound at all: see
// piAgentBwrapHiddenFiles. sandbox-exec grants by path, so its profile
// allows a write to the exact names above and to nothing else in the agent
// dir.
//
// auth.json stays writable, and pi runs an api_key value in it that starts
// with "!" as a shell command. CheckPIAuthFile (pi_auth_check.go) refuses a
// pi start that prism makes in that case. The known limits of this layout,
// and this open limit, are in docs/invariants/sandbox-pi-agent-dir.md.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const piAgentSessionsDirName = "sessions"

// piAgentCredentialFiles are the files in the agent dir that a sandboxed
// session can write in place, on both isolators. prism creates them on the
// host before the sandbox starts. In bwrap, the file bind of a missing file
// is not possible. In sandbox-exec, the profile does not allow a create or
// an unlink of these names, so that the session cannot put a symlink there.
var piAgentCredentialFiles = []string{"auth.json", "atlassian-mcp-oauth.json"}

// piAgentLockDirs are the proper-lockfile lock dirs that pi 1.4 makes in the
// root of the agent dir.
var piAgentLockDirs = []string{
	"auth.json.lock",
	"settings.json.lock",
	"trust.json.lock",
	"models-store.json.lock",
}

// The Notion MCP extension store (pi/extensions/notion/auth.ts). Only the
// sandbox-exec profile can grant these: the extension renames a temp file
// onto the token file, and a rename onto a bind mount fails.
var piNotionStoreFiles = []string{"notion-mcp-oauth.json", "notion-mcp-client.json"}

const (
	piNotionLockDirName = "notion-mcp-oauth.json.lock"
	piNotionTmpPrefix   = ".notion-mcp.tmp."
)

// piAgentBwrapHiddenFiles are host entries that the bwrap sandbox does not
// get. The Notion extension refreshes its token at session start, and
// Notion rotates the refresh token on each refresh. In bwrap the extension
// cannot save the new token: a rename onto a bind fails. The host store
// then keeps a token that Notion rotated away, and the next refresh with it
// can make Notion revoke the whole grant. With no token file, the extension
// has no tokens and sends no refresh. #3290 tracks a store that bwrap can
// write. notion-mcp-client.json stays bound read-only, so that a login
// reuses the client registration: a new registration orphans the grants of
// the old one.
var piAgentBwrapHiddenFiles = []string{"notion-mcp-oauth.json"}

// preparePIAgentDir makes, on the host, the agent-dir paths that a
// sandboxed session needs before it starts: the sessions dir of worktree,
// and the credential files (empty, mode 0600, if absent). An empty
// auth.json is valid for pi and for `prism account`. A failure is logged,
// and the session still starts. Then, in bwrap, the bind of that path is
// left out: pi writes the path in the private dir, and the write does not
// reach the host. In sandbox-exec, pi gets a write error.
func preparePIAgentDir(hostDir, worktree string) {
	if hostDir == "" {
		return
	}
	if dir := piAgentSessionDir(hostDir, worktree); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Printf("container: pi: create session dir %s: %v", dir, err)
		}
	}
	for _, name := range piAgentCredentialFiles {
		path := filepath.Join(hostDir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			continue
		}
		if !errors.Is(err, os.ErrExist) {
			log.Printf("container: pi: create %s: %v", path, err)
		}
	}
}

// piAgentSessionDir returns the host path of the pi sessions dir of
// worktree: <hostDir>/sessions/<encoded-cwd>. It returns "" when worktree is
// empty.
func piAgentSessionDir(hostDir, worktree string) string {
	if worktree == "" {
		return ""
	}
	return filepath.Join(hostDir, piAgentSessionsDirName, encodePiCWD(filepath.Clean(worktree)))
}

// piAgentBwrapRWFile reports whether name is a credential file that the
// bwrap sandbox binds read-write.
func piAgentBwrapRWFile(name string) bool {
	for _, f := range piAgentCredentialFiles {
		if name == f {
			return true
		}
	}
	return false
}

// appendPIAgentDirBwrapMounts appends the bwrap args that make the agent
// dir at sandboxDir:
//
//  1. --dir sandboxDir: a private dir in the tmpfs root of bwrap.
//  2. --ro-bind-try for each entry of hostDir, except sessions/, the
//     credential files, *.lock, and piAgentBwrapHiddenFiles. A host lock dir
//     is not bound: pi in the sandbox cannot remove a stale one from a
//     read-only bind.
//  3. --bind for each credential file that is a regular file on the host.
//  4. --bind for <hostDir>/sessions/<encoded worktree>, the only sessions
//     dir that the sandbox can see.
//
// The entries are read when the sandbox starts. A host entry that appears
// later is not visible in the sandbox, and a read-only bind keeps the file
// that it had at the start.
func appendPIAgentDirBwrapMounts(args []string, hostDir, sandboxDir, worktree string) ([]string, error) {
	preparePIAgentDir(hostDir, worktree)

	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return nil, fmt.Errorf("pi: read agent dir %s: %w", hostDir, err)
	}

	args = append(args, "--dir", filepath.Dir(sandboxDir), "--dir", sandboxDir)
	for _, e := range entries {
		name := e.Name()
		if name == piAgentSessionsDirName || piAgentBwrapRWFile(name) || strings.HasSuffix(name, ".lock") ||
			slices.Contains(piAgentBwrapHiddenFiles, name) {
			continue
		}
		args = append(args, "--ro-bind-try", filepath.Join(hostDir, name), filepath.Join(sandboxDir, name))
	}
	for _, name := range piAgentCredentialFiles {
		host := filepath.Join(hostDir, name)
		if fi, statErr := os.Lstat(host); statErr == nil && fi.Mode().IsRegular() {
			args = append(args, "--bind", host, filepath.Join(sandboxDir, name))
		}
	}
	if hostSessions := piAgentSessionDir(hostDir, worktree); hostSessions != "" {
		if fi, statErr := os.Lstat(hostSessions); statErr == nil && fi.IsDir() {
			sandboxSessions := filepath.Join(sandboxDir, piAgentSessionsDirName)
			args = append(args,
				"--dir", sandboxSessions,
				"--bind", hostSessions, filepath.Join(sandboxSessions, filepath.Base(hostSessions)))
		}
	}
	return args, nil
}

// piAgentDirSBPLAllow returns the section-6a clauses of the sandbox-exec
// profile for agentDir: a read of the whole dir, and a write of the exact
// paths in the header of this file. The sessions grant covers the sessions
// dir of worktree only.
func piAgentDirSBPLAllow(agentDir, worktree string) string {
	var b strings.Builder
	b.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
	b.WriteString("  (subpath " + quoteSBPL(agentDir) + "))\n")

	var rules []string
	for _, name := range piAgentCredentialFiles {
		rules = append(rules, "(literal "+quoteSBPL(filepath.Join(agentDir, name))+")")
	}
	for _, name := range piAgentLockDirs {
		rules = append(rules, "(literal "+quoteSBPL(filepath.Join(agentDir, name))+")")
	}
	for _, name := range piNotionStoreFiles {
		rules = append(rules, "(literal "+quoteSBPL(filepath.Join(agentDir, name))+")")
	}
	rules = append(rules,
		"(subpath "+quoteSBPL(filepath.Join(agentDir, piNotionLockDirName))+")",
		"(regex #\"^"+regexQuotePath(filepath.Join(agentDir, piNotionTmpPrefix))+"[^/]+$\")")
	if sessions := piAgentSessionDir(agentDir, worktree); sessions != "" {
		rules = append(rules, "(subpath "+quoteSBPL(sessions)+")")
	}
	b.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
	b.WriteString("  " + strings.Join(rules, "\n  ") + ")\n")
	return b.String()
}

// piAgentDirSBPLDeny returns the section-21f clause: no create and no
// unlink of the credential files. pi and the Atlassian extension write
// them in place. Without this deny, the session can replace auth.json with
// a symlink, and a host pi then writes its tokens to the target of the
// symlink.
func piAgentDirSBPLDeny(agentDir string) string {
	clause := "(deny file-write-unlink file-write-create"
	for _, name := range piAgentCredentialFiles {
		clause += "\n  (literal " + quoteSBPL(filepath.Join(agentDir, name)) + ")"
	}
	return clause + ")\n"
}
