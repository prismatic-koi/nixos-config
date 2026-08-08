package cmd

// prism exporter — long-running host daemon serving prism's own operational
// metrics on /metrics for Alloy to scrape (issue #2700, parent #2699).
//
// The daemon runs on the HOST. prism.db is not bind-mounted into worker
// sandboxes, so there is no in-sandbox proxy path for this command and none
// is wanted.

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/exporter"
)

var (
	exporterFlagListen    string
	exporterFlagPort      int
	exporterFlagDBPath    string
	exporterFlagStatePath string
)

var exporterCmd = &cobra.Command{
	Use:   "exporter",
	Short: "Serve prism operational metrics on /metrics for Prometheus",
	Long: `Serve prism operational metrics on /metrics for Prometheus.

A long-running host daemon. It opens prism.db read-only and serves the
Prometheus text exposition format on ` + exporter.MetricsPath + `.

Two kinds of metric, produced two different ways:

  * Gauges are recomputed on every scrape. They are point-in-time by
    definition and carry no monotonicity contract.

  * Counters are produced by tailing agent_events forward by its monotonic
    rowid and accumulating in memory. The cursor and the accumulated values
    are persisted together, atomically, so a restart resumes exactly where
    it stopped.

Counters are never computed as a full-table aggregate. prism prunes
agent_events at 90 days, so a SELECT COUNT(*) counter would decrease at the
prune horizon, Prometheus would read that as a counter reset, and rate()
would return wrong numbers across the boundary with no error anywhere.

On a first run — no state file — the daemon starts at the current maximum
rowid and does not backfill history. A corrupt or truncated state file is
logged and treated the same way; it never stops the daemon.

The daemon binds loopback by default. It has no authentication of its own,
so it is not meant to be reachable from the network — Alloy scrapes it from
the same host.`,
	Args: cobra.NoArgs,
	RunE: runExporter,
}

func init() {
	exporterCmd.Flags().StringVar(&exporterFlagListen, "listen", exporter.DefaultListenHost,
		"address to bind (default loopback; the endpoint is unauthenticated)")
	exporterCmd.Flags().IntVar(&exporterFlagPort, "port", exporter.DefaultPort,
		"TCP port to serve "+exporter.MetricsPath+" on")
	exporterCmd.Flags().StringVar(&exporterFlagDBPath, "db", "",
		"path to prism.db (default $XDG_STATE_HOME/prism/prism.db)")
	exporterCmd.Flags().StringVar(&exporterFlagStatePath, "state", "",
		"path to the tail-cursor state file (default alongside prism.db)")
	rootCmd.AddCommand(exporterCmd)
}

func runExporter(cmd *cobra.Command, _ []string) error {
	if exporterFlagPort < 0 || exporterFlagPort > 65535 {
		return fmt.Errorf("--port must be between 0 and 65535, got %d", exporterFlagPort)
	}

	dbFile := exporterFlagDBPath
	if dbFile == "" {
		dbFile = dbPath()
	}
	statePath := exporterFlagStatePath
	if statePath == "" {
		statePath = exporter.DefaultStatePath(dbFile)
	}

	e, err := exporter.New(exporter.Config{
		DBPath:     dbFile,
		StatePath:  statePath,
		ListenAddr: net.JoinHostPort(exporterFlagListen, strconv.Itoa(exporterFlagPort)),
		Logger:     log.New(cmd.ErrOrStderr(), "prism-exporter: ", log.LstdFlags),
	})
	if err != nil {
		return err
	}

	// SIGINT / SIGTERM cancel the context, which drains in-flight scrapes
	// and writes one last state snapshot before Run returns. systemd sends
	// SIGTERM on stop and on restart.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return e.Run(ctx)
}
