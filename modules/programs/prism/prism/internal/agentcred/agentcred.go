// Package agentcred decides which agent credentials a session gets
// (nx.programs.prism.agentCredentials).
//
// agent-credentials.json holds each credential: the repos whose sessions get
// it, and three kinds of environment variable.
//
//   - A file variable holds the path of a decrypted secret file. No variable
//     holds the content. bwrap binds the file read-only, the sandbox-exec
//     profile admits a read of the file and no write, and the redactor masks
//     the content as [redacted:<marker>].
//   - A value variable holds the content of a decrypted secret file, read on
//     the host.
//   - A literal variable holds a fixed string from the nix option.
//
// The session also gets payload.CredentialFilesEnvName, which lists the file
// variables and their markers. The pi extension in the session cannot read
// agent-credentials.json, so this variable is how its redactor knows which
// files to read.
//
// The repo comes from projecttrust.Policy.SessionProject: the session name,
// the start directory, and the project roots in config.json. No sandbox can
// read or write config.json or agent-credentials.json.
//
// A review agent gets no credential: it reviews a diff on a read-only
// worktree. An investigator gets the credentials of its repo, as a
// coordinator and a worker do: it answers questions about the live system.
//
// ForSession checks each credential on its own. If a secret file is missing,
// if a file variable holds a value that the redactor cannot mask
// (payload.UsableSecretValue), or if a value is not one token, the session
// gets no variable of that credential, and ForSession gives a warning that
// names the credential and the cause. The session gets the other credentials,
// and it starts in every case.
//
// Host mode has no filesystem isolation. There the variables are the only
// control: a host-mode session of any repo can read the sops path.
package agentcred

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/projecttrust"
)

// reservedEnvPrefix starts the names of the variables that prism sets. No
// credential can declare such a name, so no credential can replace
// payload.CredentialFilesEnvName or a variable of the harness.
const reservedEnvPrefix = "PRISM_"

// reviewRoles are the five roles of the canonical review set (internal/review
// Agents()). They are listed here because internal/review imports a package
// that imports this one. A fallback reviewer runs with the role of the agent
// that it replaces.
var reviewRoles = map[string]bool{
	"review-goal":     true,
	"review-code":     true,
	"review-context":  true,
	"review-qa":       true,
	"review-security": true,
}

// File is a file variable of a delivered credential.
type File struct {
	// Env is the name of the variable that holds the path.
	Env string
	// Path is the host path of the secret file. It is also the path in a
	// bwrap or sandbox-exec sandbox.
	Path string
	// Marker is the marker name of the content of the file.
	Marker string
}

// Credential is one delivered credential.
type Credential struct {
	// Name is the name of the credential in the nix option.
	Name string
	// Files are the file variables, in the order of their names.
	Files []File
	// Vars are the value and literal variables, with their values.
	Vars map[string]string
}

// Delivery is what a session gets. The zero value gives nothing.
type Delivery struct {
	// Credentials are in the order of their names.
	Credentials []Credential
}

// Enabled reports whether the session gets a credential.
func (d Delivery) Enabled() bool { return len(d.Credentials) > 0 }

// Files returns the file variables of all credentials of the delivery.
func (d Delivery) Files() []File {
	var out []File
	for _, c := range d.Credentials {
		out = append(out, c.Files...)
	}
	return out
}

// EnvVars returns the variables of the delivery, or nil when it gives
// nothing. When a credential has a file variable, the map also holds
// payload.CredentialFilesEnvName.
func (d Delivery) EnvVars() map[string]string {
	if !d.Enabled() {
		return nil
	}
	out := map[string]string{}
	registry := map[string]string{}
	for _, c := range d.Credentials {
		maps.Copy(out, c.Vars)
		for _, f := range c.Files {
			out[f.Env] = f.Path
			registry[f.Env] = f.Marker
		}
	}
	if len(registry) > 0 {
		out[payload.CredentialFilesEnvName] = payload.EncodeCredentialFiles(registry)
	}
	return out
}

// ForSession returns the delivery for the session sessionName with role that
// starts in dir. cfg gives the project roots. It calls warn once for each
// problem, and never with the value of a secret. It reads
// agent-credentials.json each time, so a change takes effect at the next start
// of a session.
func ForSession(sessionName, role, dir string, cfg config.Config, warn func(string)) Delivery {
	if reviewRoles[role] {
		return Delivery{}
	}
	policy := projecttrust.Policy{ProjectLocations: cfg.ProjectLocations, ProjectSpecific: cfg.ProjectSpecific}
	root, ok := policy.SessionProject(sessionName, dir)
	if !ok {
		return Delivery{}
	}
	all, err := config.LoadAgentCredentials()
	if err != nil {
		warn(fmt.Sprintf("%v — the session gets no agent credential", err))
		return Delivery{}
	}
	repo := filepath.Base(root)
	selected := map[string]config.AgentCredential{}
	for name, c := range all {
		if slices.Contains(c.Repos, repo) {
			selected[name] = c
		}
	}
	conflicts := envNameConflicts(selected)
	var d Delivery
	for _, name := range slices.Sorted(maps.Keys(selected)) {
		c := selected[name]
		if others, ok := conflicts[name]; ok {
			warn(fmt.Sprintf("agent credential %s: it declares a variable that the credential %s also declares — %s",
				name, strings.Join(others, " and "), noVariables(declaredEnvNames(c))))
			continue
		}
		got, err := resolve(name, c)
		if err != nil {
			warn(fmt.Sprintf("agent credential %s: %v — %s", name, err, noVariables(declaredEnvNames(c))))
			continue
		}
		d.Credentials = append(d.Credentials, got)
	}
	return d
}

// noVariables ends each warning. It names the variables that the session
// does not get.
func noVariables(names []string) string {
	if len(names) == 0 {
		return "the session gets no variable of this credential"
	}
	return "the session gets no variable of this credential: " + strings.Join(names, ", ")
}

// declaredEnvNames returns the names of the variables that c declares,
// sorted and with no duplicates.
func declaredEnvNames(c config.AgentCredential) []string {
	names := slices.Collect(maps.Keys(c.Files))
	names = slices.AppendSeq(names, maps.Keys(c.Values))
	names = slices.AppendSeq(names, maps.Keys(c.Literals))
	slices.Sort(names)
	return slices.Compact(names)
}

// envNameConflicts returns, for each credential that declares a variable
// that another credential also declares, the names of those other
// credentials, sorted. The nix option refuses such a pair, so this is a
// check of a hand-edited file.
func envNameConflicts(creds map[string]config.AgentCredential) map[string][]string {
	owners := map[string][]string{}
	for name, c := range creds {
		for _, env := range declaredEnvNames(c) {
			owners[env] = append(owners[env], name)
		}
	}
	out := map[string][]string{}
	for _, names := range owners {
		if len(names) < 2 {
			continue
		}
		for _, n := range names {
			for _, other := range names {
				if other != n && !slices.Contains(out[n], other) {
					out[n] = append(out[n], other)
				}
			}
		}
	}
	for n := range out {
		slices.Sort(out[n])
	}
	return out
}

// resolve checks the variables of one credential, and reads its value files.
// The errors name a variable, a path, and a reason, never a secret value.
func resolve(name string, c config.AgentCredential) (Credential, error) {
	kinds := map[string]int{}
	for env := range c.Files {
		kinds[env]++
	}
	for env := range c.Values {
		kinds[env]++
	}
	for env := range c.Literals {
		kinds[env]++
	}
	for _, env := range slices.Sorted(maps.Keys(kinds)) {
		if !payload.IsCredentialFileName(env) || strings.HasPrefix(env, reservedEnvPrefix) {
			return Credential{}, fmt.Errorf("the variable name %q does not match %s, or starts with %s", env, payload.CredentialFileNamePattern, reservedEnvPrefix)
		}
		if kinds[env] > 1 {
			return Credential{}, fmt.Errorf("the variable %s is declared more than once", env)
		}
	}

	out := Credential{Name: name, Vars: map[string]string{}}
	for _, env := range slices.Sorted(maps.Keys(c.Files)) {
		f := c.Files[env]
		if !payload.IsCredentialFileName(f.Marker) {
			return Credential{}, fmt.Errorf("%s: the marker %q does not match %s", env, f.Marker, payload.CredentialFileNamePattern)
		}
		if !filepath.IsAbs(f.Path) {
			return Credential{}, fmt.Errorf("%s: the path %q is not absolute", env, f.Path)
		}
		v, err := payload.ReadSecretFile(f.Path)
		if err != nil {
			return Credential{}, fmt.Errorf("%s: %w", env, err)
		}
		if !payload.UsableSecretValue(v) {
			return Credential{}, fmt.Errorf("%s: the redactor cannot mask the value in the file %s: it is empty, holds fewer than %d characters, or holds a $( literal", env, f.Path, payload.MinCredentialValueLen)
		}
		out.Files = append(out.Files, File{Env: env, Path: f.Path, Marker: f.Marker})
	}
	for _, env := range slices.Sorted(maps.Keys(c.Values)) {
		path := c.Values[env].Path
		if !filepath.IsAbs(path) {
			return Credential{}, fmt.Errorf("%s: the path %q is not absolute", env, path)
		}
		v, err := payload.ReadSecretFile(path)
		if err != nil {
			return Credential{}, fmt.Errorf("%s: %w", env, err)
		}
		if !isOneToken(v) {
			return Credential{}, fmt.Errorf("%s: the file %s is empty or holds a space or a control character", env, path)
		}
		out.Vars[env] = v
	}
	for _, env := range slices.Sorted(maps.Keys(c.Literals)) {
		v := c.Literals[env]
		if !isOneToken(v) {
			return Credential{}, fmt.Errorf("%s: the literal value is empty or holds a space or a control character", env)
		}
		out.Vars[env] = v
	}
	return out, nil
}

// isOneToken reports whether v is not empty and holds no space and no
// control character. A value or a literal is one token, for example a host
// name or a user name. A space or a control character shows a broken secret
// file or a wrong declaration, and a newline can split a header line.
func isOneToken(v string) bool {
	return v != "" && !strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r == 0x7f })
}
