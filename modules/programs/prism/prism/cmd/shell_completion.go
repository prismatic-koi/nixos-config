package cmd

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/account"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
)

// The completion functions run on each Tab press. Each one fails quiet: a
// missing or unreadable source gives no candidates and no output, because
// any byte outside the cobra completion protocol corrupts the shell prompt.

// This file must sort after cleanup.go: Go runs the init functions of a
// package in file-name order, and RegisterFlagCompletionFunc needs the
// --session flag that the init in cleanup.go defines.
func init() {
	for _, c := range []*cobra.Command{accountUseCmd, accountRmCmd, accountLoginCmd} {
		c.ValidArgsFunction = completeAccountNames
	}
	for _, c := range []*cobra.Command{profileUseCmd, profileShowCmd} {
		c.ValidArgsFunction = completeProfileNames
	}
	for _, c := range []*cobra.Command{checkinCmd, logsCmd, promptCmd, statsCmd, archiveCmd} {
		c.ValidArgsFunction = completeSessionNames
	}
	if err := cleanupCmd.RegisterFlagCompletionFunc("session", completeSessionFlag); err != nil {
		panic(err)
	}
}

func completeAccountNames(_ *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return filterPrefix(accountNameCandidates(), toComplete), cobra.ShellCompDirectiveNoFileComp
}

// accountNameCandidates uses account.List and not account.Init: Init creates
// the store and runs a migration. List reads the directory entries only, so
// no account file is opened.
func accountNameCandidates() []string {
	p, err := account.ResolvePaths()
	if err != nil {
		return nil
	}
	names, err := account.List(p)
	if err != nil {
		return nil
	}
	return names
}

func completeProfileNames(_ *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	pf, err := config.LoadProfiles()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return filterPrefix(config.AvailableProfileNames(pf), toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeSessionNames(_ *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return filterPrefix(sessionNameCandidates(dbPath()), toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeSessionFlag(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return filterPrefix(sessionNameCandidates(dbPath()), toComplete), cobra.ShellCompDirectiveNoFileComp
}

// sessionNameCandidates uses db.OpenReadOnly and not db.Open: db.Open
// creates the file when it is missing and runs the migrations.
func sessionNameCandidates(path string) []string {
	conn, err := db.OpenReadOnly(path)
	if err != nil {
		return nil
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := conn.QueryContext(ctx,
		`SELECT DISTINCT session_name FROM sessions ORDER BY session_name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil
		}
		names = append(names, n)
	}
	if rows.Err() != nil {
		return nil
	}
	return names
}

func filterPrefix(candidates []string, prefix string) []cobra.Completion {
	var out []cobra.Completion
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}
