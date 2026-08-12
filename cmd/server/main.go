package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"github.com/itcaat/teamcity-mcp/internal/config"
	"github.com/itcaat/teamcity-mcp/internal/logging"
	"github.com/itcaat/teamcity-mcp/internal/metrics"
	"github.com/itcaat/teamcity-mcp/internal/server"
	"github.com/itcaat/teamcity-mcp/internal/teamcity"
)

var (
	transport   = flag.String("transport", "http", "Transport mode: http or stdio")
	versionFlag = flag.Bool("version", false, "Show version information")
	envHelp     = flag.Bool("help", false, "Show environment variable help")

	// Build-time variables set by GoReleaser
	version = "dev"
	commit  = "none"
	date    = "unknown"
	builtBy = "unknown"
)

const (
	appName = "teamcity-mcp"
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nTeamCity MCP Server - connects TeamCity to AI agents via MCP protocol\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment Variables:\n")
		fmt.Fprintf(os.Stderr, "  Run '%s --help' for detailed environment variable documentation\n\n", os.Args[0])
	}
}

func main() {
	flag.Parse()

	if *versionFlag {
		fmt.Printf("%s version %s\n", appName, version)
		fmt.Printf("  commit: %s\n", commit)
		fmt.Printf("  built at: %s\n", date)
		fmt.Printf("  built by: %s\n", builtBy)
		os.Exit(0)
	}

	if *envHelp {
		config.PrintEnvHelp()
		os.Exit(0)
	}

	// Load configuration from environment variables
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Requirements differ per transport: stdio needs TC_URL and TC_TOKEN, while
	// http only needs a permitted TeamCity server for clients to authenticate
	// against. Failing here beats a server that rejects every request.
	if err := cfg.ValidateForTransport(*transport); err != nil {
		log.Fatalf("Invalid configuration for -transport %s: %v", *transport, err)
	}

	// Initialize logging
	logger, err := logging.New(cfg.Logging)
	if err != nil {
		log.Fatalf("Failed to initialize logging: %v", err)
	}
	defer logger.Sync()

	warnUnsafeConfig(logger, cfg, *transport)

	// Initialize metrics
	metrics.Init()

	// Create server
	srv, err := server.New(cfg, logger)
	if err != nil {
		logger.Fatal("Failed to create server", "error", err)
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		for sig := range sigChan {
			switch sig {
			case syscall.SIGHUP:
				logger.Info("Received SIGHUP, reloading configuration")
				if newCfg, err := config.Load(); err != nil {
					logger.Error("Failed to reload configuration", "error", err)
				} else {
					srv.UpdateConfig(newCfg)
				}
			case syscall.SIGINT, syscall.SIGTERM:
				logger.Info("Received shutdown signal", "signal", sig)
				cancel()
			}
		}
	}()

	// Start server
	fields := []interface{}{
		"Starting TeamCity MCP server",
		"version", version,
		"commit", commit,
		"transport", *transport,
	}
	if cfg.TeamCity.URL != "" {
		fields = append(fields, "teamcity_url", cfg.TeamCity.URL)
	}
	logger.Info(fields...)

	if err := srv.Start(ctx, *transport); err != nil {
		logger.Fatal("Server failed", "error", err)
	}

	logger.Info("Server shutdown complete")
}

// warnUnsafeConfig flags settings that are dangerous in a shared deployment.
func warnUnsafeConfig(logger *zap.SugaredLogger, cfg *config.Config, transport string) {
	if transport != "http" {
		return
	}

	if cfg.TeamCity.AllowAnyURL {
		logger.Warn("TC_ALLOW_ANY_URL is enabled: any client can make this server issue requests to arbitrary hosts. Do not use this in a shared deployment")
	}
	if cfg.TeamCity.AllowServerToken {
		logger.Warn("TC_ALLOW_SERVER_TOKEN_FALLBACK is enabled: requests without an " +
			teamcity.HeaderToken + " header will act as the identity that owns TC_TOKEN")
	}
	if cfg.TeamCity.Token != "" && !cfg.TeamCity.AllowServerToken {
		logger.Info("TC_TOKEN is set but only used for the /readyz probe; clients must send their own " + teamcity.HeaderToken + " header")
	}
	if cfg.Server.ServerSecret == "" {
		logger.Info("SERVER_SECRET is not set, so the outer authentication gate is disabled; anyone who can reach this port may use it with their own TeamCity token")
	}
}
