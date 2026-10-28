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
// The extensions write this path:
//
//   - atlassian-mcp-oauth.json: Atlassian MCP OAuth refresh, a write in
//     place.
//
// auth.json is not the host file. pi runs an api_key value in auth.json
// that starts with "!" as a shell command, so a sandbox must not write the
// file that later sessions read. Each sandboxed session has its own
// auth.json in a per-session host dir, and the sidecar copies OAuth entries
// between it and the host file (pi_auth_session.go, #3295).
//
// The Notion MCP store and the shared OAuth refresh lock are not in the
// agent dir. They are in dedicated dirs that every session shares
// (pi_shared_state.go, #3289, #3290).
//
// bwrap cannot give "create only these names" in a shared dir. The agent
// dir in a bwrap sandbox is therefore a private dir: bwrap makes it in its
// own tmpfs root, and it goes away with the sandbox. prism binds each host
// entry into it read-only, binds atlassian-mcp-oauth.json read-write, binds
// the sessions dir of the worktree read-write, and makes auth.json a symlink
// to the per-session auth.json. The lock dirs are private to the sandbox.
//
// sandbox-exec has no binds. Its pi agent dir is the per-session dir, with
// a symlink to each other host entry (LinkPIAgentDirEntries). pi makes its
// lock dirs there. The profile allows a write of the per-session dir, of
// atlassian-mcp-oauth.json, and of the sessions dir of the worktree, and of
// nothing else in the host agent dir.
//
// The known limits of this layout are in
// docs/invariants/sandbox-pi-agent-dir.md.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const piAgentSessionsDirName = "sessions"

// piAgentCredentialFiles are the files in the host agent dir that a
// sandboxed session can write in place, on both isolators. prism creates
// them on the host before the sandbox starts. In bwrap, the file bind of a
// missing file is not possible. In sandbox-exec, the profile does not allow
// a create or an unlink of these names, so that the session cannot put a
// symlink there. auth.json is not in the list: a session writes its own
// auth.json (pi_auth_session.go).
var piAgentCredentialFiles = []string{"atlassian-mcp-oauth.json"}

// piAuthSessionSandboxDir is the path in a bwrap sandbox of the per-session
// dir that holds auth.json (Config.PIAuthSessionDir).
const piAuthSessionSandboxDir = "/run/prism/pi-auth"

// The Notion MCP extension store (pi/extensions/notion/auth.ts). The store
// is in its own dir (pi_shared_state.go). An old store in the agent dir is
// read by the one-time move, and by the extension until the move is done.
var piNotionStoreFiles = []string{"notion-mcp-oauth.json", "notion-mcp-client.json"}

const (
	piNotionLockDirName = "notion-mcp-oauth.json.lock"
	piNotionTmpPrefix   = ".notion-mcp.tmp."
)

// preparePIAgentDir makes, on the host, the agent-dir paths that a
// sandboxed session needs before it starts: the sessions dir of worktree,
// and the credential files (empty, mode 0600, if absent). A failure is
// logged, and the session still starts. Then, in bwrap, the bind of that path is
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
//  2. --ro-bind-try for each entry of hostDir, except sessions/, auth.json,
//     the credential files, *.lock, and the temp files of the auth sync. A
//     host lock dir is not bound: pi in the sandbox cannot remove a stale one
//     from a read-only bind.
//  3. --bind for each credential file that is a regular file on the host.
//  4. --bind of authDir, the per-session dir of auth.json, at
//     piAuthSessionSandboxDir, and a symlink sandboxDir/auth.json to the
//     auth.json in it. A dir bind, not a file bind: the sync replaces the
//     file with a rename, and a file bind keeps the old file. With no
//     authDir, the sandbox has no auth.json from the host.
//  5. --bind for <hostDir>/sessions/<encoded worktree>, the only sessions
//     dir that the sandbox can see.
//
// The entries are read when the sandbox starts. A host entry that appears
// later is not visible in the sandbox, and a read-only bind keeps the file
// that it had at the start.
func appendPIAgentDirBwrapMounts(args []string, hostDir, sandboxDir, worktree, authDir string) ([]string, error) {
	preparePIAgentDir(hostDir, worktree)

	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return nil, fmt.Errorf("pi: read agent dir %s: %w", hostDir, err)
	}

	args = append(args, "--dir", filepath.Dir(sandboxDir), "--dir", sandboxDir)
	for _, e := range entries {
		name := e.Name()
		if name == piAgentSessionsDirName || name == piAuthFileName || piAgentBwrapRWFile(name) ||
			strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, piAuthTmpPrefix) {
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
	if authDir != "" {
		if fi, statErr := os.Lstat(authDir); statErr == nil && fi.IsDir() {
			args = append(args,
				"--bind", authDir, piAuthSessionSandboxDir,
				"--symlink", filepath.Join(piAuthSessionSandboxDir, piAuthFileName), filepath.Join(sandboxDir, piAuthFileName))
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
// profile for agentDir: a read of the whole dir, and a write of the
// credential files and of the sessions dir of worktree. pi makes its lock
// dirs and writes auth.json in the per-session dir (section 6d), not here.
func piAgentDirSBPLAllow(agentDir, worktree string) string {
	var b strings.Builder
	b.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
	b.WriteString("  (subpath " + quoteSBPL(agentDir) + "))\n")

	var rules []string
	for _, name := range piAgentCredentialFiles {
		rules = append(rules, "(literal "+quoteSBPL(filepath.Join(agentDir, name))+")")
	}
	if sessions := piAgentSessionDir(agentDir, worktree); sessions != "" {
		rules = append(rules, "(subpath "+quoteSBPL(sessions)+")")
	}
	b.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
	b.WriteString("  " + strings.Join(rules, "\n  ") + ")\n")
	return b.String()
}

// piAuthSessionSBPLAllow returns the section-6d clause: a read and a write
// of the per-session agent dir authDir, which holds the auth.json of the
// session, the lock dirs of pi, and the symlinks to the host entries. It
// returns "" when authDir is empty.
func piAuthSessionSBPLAllow(authDir string) string {
	if authDir == "" {
		return ""
	}
	return "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + quoteSBPL(authDir) + "))\n"
}

// piAuthSessionSBPLDeny returns the section-21h clause: no unlink, create,
// mode, flags, or acl change of the authDir node. Section 6d grants a
// subpath, which covers the node too. Without this deny, the session can
// remove the dir and put a symlink at its path, and the sync then writes
// the session auth file where the session chose. It returns "" when
// authDir is empty.
func piAuthSessionSBPLDeny(authDir string) string {
	if authDir == "" {
		return ""
	}
	return "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n  (literal " +
		quoteSBPL(authDir) + "))\n"
}

// piSharedStateSBPLAllow returns the section-6c clause: a read and a write
// of the whole lock dir and of the whole Notion store dir
// (pi_shared_state.go). An empty dir is left out. It returns "" when both
// are empty.
func piSharedStateSBPLAllow(lockDir, notionDir string) string {
	var rules []string
	for _, dir := range []string{lockDir, notionDir} {
		if dir != "" {
			rules = append(rules, "(subpath "+quoteSBPL(dir)+")")
		}
	}
	if len(rules) == 0 {
		return ""
	}
	return "(allow file-read* file-write* file-test-existence file-read-metadata\n  " +
		strings.Join(rules, "\n  ") + ")\n"
}

// piSharedStateSBPLDeny returns the section-21g clause: no unlink, create,
// mode, flags, or acl change of the two shared dir nodes. Section 6c grants
// a subpath, which covers the node too. Without this deny, a session can
// rename a shared dir away and put a symlink at its path, and a host pi and
// prism agent-run then follow it. A sandbox still writes freely in the
// dirs. It returns "" when both are empty.
func piSharedStateSBPLDeny(lockDir, notionDir string) string {
	var rules []string
	for _, dir := range []string{lockDir, notionDir} {
		if dir != "" {
			rules = append(rules, "(literal "+quoteSBPL(dir)+")")
		}
	}
	if len(rules) == 0 {
		return ""
	}
	return "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n  " +
		strings.Join(rules, "\n  ") + ")\n"
}

// piAgentDirSBPLDeny returns the section-21f clause: no create and no
// unlink of auth.json and of the credential files in the host agent dir.
// The Atlassian extension writes its file in place. Without this deny, the
// session can replace a file with a symlink, and a host pi then writes its
// tokens to the target of the symlink. Section 6a grants no write of
// auth.json, and the deny keeps that so if a later grant covers it.
func piAgentDirSBPLDeny(agentDir string) string {
	clause := "(deny file-write-unlink file-write-create"
	for _, name := range append([]string{piAuthFileName}, piAgentCredentialFiles...) {
		clause += "\n  (literal " + quoteSBPL(filepath.Join(agentDir, name)) + ")"
	}
	return clause + ")\n"
}
