package cmd

// prism agent-context — machine-readable introspection of the prism CLI shape.
//
// Emits a versioned JSON document to stdout describing every (non-hidden)
// command and its flags, available model profiles, and cross-cutting
// precedence rules. This is the layer-2 introspection surface: between
// --help (human-readable) and SKILL.md (workflow prose), agents can query
// this document to understand the exact CLI shape without parsing help text.
//
// Usage:
//
//	prism agent-context                   # emit JSON for all visible commands
//	prism agent-context --include-hidden  # also include hidden commands
//
// Output shape (top-level keys):
//
//	schema_version     — "1" (bump on breaking changes)
//	prism_version      — git SHA of the binary (empty in dev builds)
//	commands           — map of command name → CommandMeta
//	available_profiles — list of profile names from profiles.json ([] if missing)
//	precedence         — map of cross-cutting precedence rules
//
// See issue #1498.

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/prismatic-koi/prism/internal/archive"
	"github.com/prismatic-koi/prism/internal/config"
)

// agentContextSchemaVersion is the schema version string. Bump on any
// breaking change to the document shape.
const agentContextSchemaVersion = "1"

// FlagMeta describes a single CLI flag.
type FlagMeta struct {
	// Type is one of: bool, string, int, duration, stringArray, enum.
	Type string `json:"type"`
	// Values is present when Type == "enum". Lists all valid values.
	Values []string `json:"values,omitempty"`
	// Default is the flag's default value (as a string). Absent when empty.
	Default string `json:"default,omitempty"`
	// DefaultSource describes where the default comes from (e.g. a config file path).
	DefaultSource string `json:"default_source,omitempty"`
	// Required indicates whether the flag must be supplied.
	Required bool `json:"required,omitempty"`
	// Description is the flag's usage string.
	Description string `json:"description"`
	// Hidden is true when the flag is hidden (only present with --include-hidden).
	Hidden bool `json:"hidden,omitempty"`
}

// PositionalArg describes a positional argument.
type PositionalArg struct {
	// Name is a human-readable name for the argument.
	Name string `json:"name"`
	// Description describes the argument.
	Description string `json:"description,omitempty"`
	// Required indicates whether the argument must be supplied.
	Required bool `json:"required,omitempty"`
}

// CommandMeta describes a single cobra command.
type CommandMeta struct {
	// Description is the command's Short description.
	Description string `json:"description"`
	// LongDescription is the command's Long description (if different from Short).
	LongDescription string `json:"long_description,omitempty"`
	// Aliases lists any command aliases.
	Aliases []string `json:"aliases,omitempty"`
	// Flags is a map of "--flag-name" → FlagMeta.
	Flags map[string]FlagMeta `json:"flags,omitempty"`
	// PositionalArgs describes positional arguments.
	PositionalArgs []PositionalArg `json:"positional_args,omitempty"`
	// Subcommands is a recursive map of subcommand name → CommandMeta.
	Subcommands map[string]CommandMeta `json:"subcommands,omitempty"`
	// Hidden indicates the command is hidden (only present with --include-hidden).
	Hidden bool `json:"hidden,omitempty"`
}

// AgentContextDocument is the top-level output structure.
type AgentContextDocument struct {
	SchemaVersion     string                 `json:"schema_version"`
	PrismVersion      string                 `json:"prism_version"`
	Commands          map[string]CommandMeta `json:"commands"`
	AvailableProfiles []string               `json:"available_profiles"`
	Precedence        map[string][]string    `json:"precedence"`
}

// flagEnumMeta carries custom metadata for flags that have enum constraints.
// The key is "--flag-name" scoped to a cobra.Command (by use string). We
// store it keyed by (commandUse, flagName) so subcommand flags don't collide.
type flagEnumMeta struct {
	values        []string
	defaultSource string
}

// enumFlagKey is the lookup key for per-command enum flag metadata.
type enumFlagKey struct {
	// commandUse is cmd.Use (e.g. "spawn") used as a stable identifier.
	commandUse string
	// flagName is the flag name without leading "--".
	flagName string
}

// enumFlagRegistry maps (commandUse, flagName) to custom metadata for
// flags whose valid values are constrained to an enum.
//
// This is the single source of truth for enum value lists: the same
// constants are consumed by both the runtime validation code and the
// agent-context generator. Do not duplicate these lists elsewhere.
var enumFlagRegistry = map[enumFlagKey]flagEnumMeta{
	{commandUse: "spawn", flagName: "isolation"}: {
		values: func() []string {
			// Share the exact same slice as the runtime validation path.
			s := make([]string, len(config.ValidIsolationModes))
			for i, m := range config.ValidIsolationModes {
				s[i] = string(m)
			}
			return s
		}(),
		defaultSource: "~/.config/prism/config.json",
	},
	{commandUse: "sidecar", flagName: "isolation-mode"}: {
		values: func() []string {
			s := make([]string, len(config.ValidIsolationModes))
			for i, m := range config.ValidIsolationModes {
				s[i] = string(m)
			}
			return s
		}(),
		defaultSource: "~/.config/prism/config.json",
	},
}

// flagDefaultSourceRegistry carries override default_source annotations for
// non-enum flags where the default comes from a config file rather than being
// a hard-coded Go default.
var flagDefaultSourceRegistry = map[enumFlagKey]string{
	{commandUse: "spawn", flagName: "profile"}: "~/.config/prism/profiles.json",
}

// precedenceRules documents the cross-cutting precedence chains for key
// configuration axes. The values are ordered highest-to-lowest precedence.
var precedenceRules = map[string][]string{
	"profile": {
		"--profile flag on prism spawn",
		"$XDG_STATE_HOME/prism/active-profile (set via prism profile use)",
		"profiles.json default field (lowest)",
	},
	"isolation": {
		"--isolation flag on prism spawn",
		"~/.config/prism/config.json default_isolation_mode field",
		"compiled-in default (host)",
	},
	// Model and provider are separate axes. They are listed apart because a
	// value on one axis does not select the other, and because pi treats a
	// mismatch between them as a warning, not an error: pi strips a --model
	// value's "<provider>/" prefix only when the prefix equals --provider. On
	// a mismatch pi builds a custom model id instead. Keep the two in
	// agreement (issue #2852).
	//
	// The model axis has three rungs, not two: --model-override names a single
	// role and beats --model for that role only.
	//
	// The top rung is enforced at the point each isolation mode renders pi's
	// --model argument (issue #2863). session.roleModelOverride picks the
	// entry keyed by the session's own role; host mode emits it from
	// buildDirectAgentCmd, and bwrap / sandbox-exec carry it as
	// `prism agent-run --agent-model` into container.Config.AgentModel, which
	// PIInvocation ranks above PIModel. Keep this string in step with those
	// three sites: agents read it as the contract for the model axis, so a rung
	// that no emit site enforces is a false statement to every one of them.
	//
	// The provider axis has two rungs because --model-override has no
	// provider-only form. It can still reach the provider axis indirectly: the
	// role=model format admits a "<provider>/" model-id prefix, so
	// --model-override <role>=openrouter/foo alongside --provider anthropic
	// produces exactly the prefix mismatch described above, for that one role.
	// That is an interaction, not a rung, so it stays in this comment rather
	// than in the ordered chain below.
	"model": {
		"--model-override role=model flag on prism spawn (that role only)",
		"--model flag on prism spawn",
		"profile slot model field in ~/.config/prism/profiles.json (lowest)",
	},
	"provider": {
		"--provider flag on prism spawn (pi harness only)",
		"profile slot provider field in ~/.config/prism/profiles.json (lowest)",
	},
}

var agentContextCmd = &cobra.Command{
	Use:   "agent-context",
	Short: "Emit a versioned JSON document describing the full prism CLI shape",
	Long: `Emit a versioned JSON document to stdout describing the full prism CLI shape.

The document includes every top-level command (and its subcommands), all
registered flags with types and enum values where applicable, the list of
available model profiles, and cross-cutting precedence rules.

This is the layer-2 introspection surface: between --help (human-readable)
and SKILL.md (workflow prose). Use it to discover the exact CLI shape
without parsing help text.

Hidden commands (cobra Hidden: true) are omitted by default. Use
--include-hidden to expose them for advanced inspection.

Output goes to stdout. Exit code is 0 on success.`,
	RunE: runAgentContext,
}

func init() {
	agentContextCmd.Flags().Bool("include-hidden", false, "Include hidden commands and flags in the output")
	rootCmd.AddCommand(agentContextCmd)
}

func runAgentContext(cmd *cobra.Command, args []string) error {
	includeHidden, _ := cmd.Flags().GetBool("include-hidden")

	doc := buildAgentContextDocument(includeHidden)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// buildAgentContextDocument constructs the AgentContextDocument by walking
// the cobra command tree. It is extracted from runAgentContext to make it
// directly testable without invoking the cobra flag machinery.
func buildAgentContextDocument(includeHidden bool) AgentContextDocument {
	// Load available profiles — missing file is not an error.
	profiles := loadAvailableProfiles()

	// Walk the command tree.
	commands := make(map[string]CommandMeta)
	for _, sub := range rootCmd.Commands() {
		if sub.Hidden && !includeHidden {
			continue
		}
		meta := buildCommandMeta(sub, includeHidden)
		commands[sub.Name()] = meta
	}

	return AgentContextDocument{
		SchemaVersion:     agentContextSchemaVersion,
		PrismVersion:      archive.PrismGitSHA(),
		Commands:          commands,
		AvailableProfiles: profiles,
		Precedence:        precedenceRules,
	}
}

// buildCommandMeta recursively constructs a CommandMeta for a cobra command.
func buildCommandMeta(cmd *cobra.Command, includeHidden bool) CommandMeta {
	meta := CommandMeta{
		Description: cmd.Short,
	}
	if cmd.Long != "" && cmd.Long != cmd.Short {
		meta.LongDescription = cmd.Long
	}
	if len(cmd.Aliases) > 0 {
		meta.Aliases = cmd.Aliases
	}
	if cmd.Hidden {
		meta.Hidden = true
	}

	// Positional args from the Use string.
	meta.PositionalArgs = extractPositionalArgs(cmd)

	// Flags — only local flags, not inherited persistent flags from parent.
	flags := make(map[string]FlagMeta)
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden && !includeHidden {
			return
		}
		flags["--"+f.Name] = buildFlagMeta(cmd, f, includeHidden)
	})
	if len(flags) > 0 {
		meta.Flags = flags
	}

	// Subcommands.
	subcommands := make(map[string]CommandMeta)
	for _, sub := range cmd.Commands() {
		if sub.Hidden && !includeHidden {
			continue
		}
		subcommands[sub.Name()] = buildCommandMeta(sub, includeHidden)
	}
	if len(subcommands) > 0 {
		meta.Subcommands = subcommands
	}

	return meta
}

// buildFlagMeta constructs a FlagMeta for a single pflag.Flag.
func buildFlagMeta(cmd *cobra.Command, f *pflag.Flag, includeHidden bool) FlagMeta {
	key := enumFlagKey{commandUse: cmd.Use, flagName: f.Name}

	// Check if this flag has enum metadata.
	if em, ok := enumFlagRegistry[key]; ok {
		fm := FlagMeta{
			Type:        "enum",
			Values:      em.values,
			Description: f.Usage,
		}
		if em.defaultSource != "" {
			fm.DefaultSource = em.defaultSource
		}
		if f.DefValue != "" {
			fm.Default = f.DefValue
		}
		if f.Hidden {
			fm.Hidden = true
		}
		return fm
	}

	fm := FlagMeta{
		Type:        cobraFlagType(f),
		Description: f.Usage,
	}
	if f.DefValue != "" && f.DefValue != "[]" {
		fm.Default = f.DefValue
	}
	// Check for a default_source annotation.
	if ds, ok := flagDefaultSourceRegistry[key]; ok {
		fm.DefaultSource = ds
	}
	// Check if the flag is required.
	if required, ok := f.Annotations[cobra.BashCompOneRequiredFlag]; ok && len(required) > 0 {
		fm.Required = true
	}
	if f.Hidden {
		fm.Hidden = true
	}
	return fm
}

// cobraFlagType maps a pflag type name to one of the canonical type strings:
// bool | string | int | duration | stringArray | enum.
func cobraFlagType(f *pflag.Flag) string {
	switch f.Value.Type() {
	case "bool":
		return "bool"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		return "int"
	case "duration":
		return "duration"
	case "stringArray", "stringSlice":
		return "stringArray"
	default:
		return "string"
	}
}

// extractPositionalArgs parses the Use field to extract positional arg names.
// e.g. "spawn [flags]" → []
//
//	"checkin <session>" → [{Name: "session"}]
//	"audit [session]"   → [{Name: "session", Required: false}]
func extractPositionalArgs(cmd *cobra.Command) []PositionalArg {
	use := cmd.Use
	// Strip the command name (first word).
	idx := strings.Index(use, " ")
	if idx < 0 {
		return nil
	}
	rest := strings.TrimSpace(use[idx+1:])
	if rest == "" {
		return nil
	}

	var args []PositionalArg
	// Walk token by token: <name> = required, [name] = optional.
	for _, tok := range strings.Fields(rest) {
		if strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">") {
			name := tok[1 : len(tok)-1]
			args = append(args, PositionalArg{Name: name, Required: true})
		} else if strings.HasPrefix(tok, "[") && strings.HasSuffix(tok, "]") {
			name := tok[1 : len(tok)-1]
			if name == "flags" || name == "command" || name == "subcommand" {
				// Skip meta-tokens that aren't real arguments.
				continue
			}
			args = append(args, PositionalArg{Name: name, Required: false})
		}
		// Other tokens (e.g. "..." or literals) are skipped.
	}
	return args
}

// loadAvailableProfiles reads profiles.json and returns the profile names.
// Returns an empty (non-nil) slice if the file is missing or unreadable.
func loadAvailableProfiles() []string {
	pf, err := config.LoadProfiles()
	if err != nil {
		// Missing file or parse error — return empty slice, not an error.
		return []string{}
	}
	names := config.AvailableProfileNames(pf)
	if names == nil {
		return []string{}
	}
	return names
}
