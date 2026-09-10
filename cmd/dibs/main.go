// Command dibs watches a curated set of GitHub repositories and surfaces
// newly opened, unclaimed issues.
//
// Dibs holds a read-only GitHub token and never writes to GitHub. Claiming
// happens in your browser, by hand.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Viswalahiri/dibs/internal/app"
	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/notify"
	"github.com/Viswalahiri/dibs/internal/reaper"
	"github.com/Viswalahiri/dibs/internal/store"
)

const usage = "usage: dibs <run|status>"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dibs:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "status":
		return cmdStatus(args[1:])
	default:
		return fmt.Errorf("unknown command %q; %s", args[0], usage)
	}
}

// loaded is everything a subcommand needs from disk and the environment.
type loaded struct {
	paths   config.Paths
	cfg     *config.Config
	repos   []config.Repo
	secrets config.Secrets
	store   *store.Store
}

func load(required ...string) (*loaded, error) {
	paths, err := config.ResolvePaths()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths.Config)
	if err != nil {
		return nil, err
	}
	repos, err := config.LoadRepos(paths.Repos)
	if err != nil {
		return nil, err
	}
	l := &loaded{paths: paths, cfg: cfg, repos: repos}
	if len(required) > 0 {
		if l.secrets, err = config.LoadSecrets(required...); err != nil {
			return nil, err
		}
	}
	if l.store, err = store.Open(paths.DB); err != nil {
		return nil, err
	}
	return l, nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	debug := fs.Bool("debug", false, "log at debug level")
	dryRun := fs.Bool("dry-run", false,
		"print alerts to stdout instead of Slack; needs no Slack tokens")
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	required := []string{config.EnvGitHubToken}
	if !*dryRun {
		required = append(required, config.EnvSlackBotToken)
	}
	l, err := load(required...)
	if err != nil {
		return err
	}
	defer l.store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var clientOpts []gh.Option
	// DIBS_GITHUB_API points dibs at a different API host. It exists so the
	// binary can be exercised against a stub without a real token.
	if base := os.Getenv("DIBS_GITHUB_API"); base != "" {
		clientOpts = append(clientOpts, gh.WithBaseURL(base))
		log.Warn("using a non-default GitHub API host", "base_url", base)
	}
	client := gh.New(l.secrets.GitHubToken, l.cfg.Polling.MaxConcurrent, clientOpts...)
	if err := assertGitHubAccess(ctx, client, l.cfg, log); err != nil {
		return err
	}

	if err := l.store.SyncRepos(ctx, l.repos, l.cfg.Polling.DefaultIntervalSec); err != nil {
		return fmt.Errorf("sync repos: %w", err)
	}

	// A crash leaves rows leased to a worker that no longer exists. Clearing
	// them here is the whole of crash recovery.
	released, err := l.store.ReleaseExpiredLeases(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("release expired leases: %w", err)
	}
	if released > 0 {
		log.Info("released leases held by a previous run", "count", released)
	}

	go watchReload(ctx, l, log)

	warn := newWarner(ctx, l.store, log)
	poller := gh.NewPoller(client, l.store, l.cfg, log, warn)
	enricher := gh.NewEnricher(client, l.store, l.cfg, log)
	pusher := notify.NewPusher(client, l.store, l.cfg, log)
	reap := reaper.New(l.store, l.cfg, log, warn)

	var transport notify.Transport
	if *dryRun {
		log.Warn("dry run: alerts go to stdout, no Slack connection")
		transport = notify.Console{}
	} else {
		transport = notify.NewSlack(l.secrets.SlackBotToken, l.cfg.Slack.DeliverTo, log)
	}
	sender := notify.NewSender(transport, l.store, log)

	workers := []app.Worker{
		{Name: "poller", Run: poller.Run},
		{Name: "enricher", Run: enricher.Run},
		{Name: "pusher", Run: pusher.Run},
		{Name: "sender", Run: sender.Run},
		{Name: "reaper", Run: reap.Run},
	}

	log.Info("dibs started", "repos", len(l.repos), "db", l.paths.DB, "dry_run", *dryRun)
	if err := app.Supervise(ctx, log, workers...); err != nil {
		return err
	}
	log.Info("dibs stopped")
	return nil
}

// newWarner routes operational conditions into the outbox, where the sender
// picks them up like any other message. The dedupe key is what keeps a paused
// poller from filling the channel.
func newWarner(ctx context.Context, s *store.Store, log *slog.Logger) gh.Warner {
	return func(kind, message string) {
		log.Warn(message, "kind", kind)
		payload, err := notify.WarningPayload(message)
		if err != nil {
			log.Error("encode warning", "err", err)
			return
		}
		if _, err := s.Enqueue(ctx, store.Outgoing{
			Kind:      notify.KindWarning,
			DedupeKey: kind + ":" + message,
			Payload:   payload,
		}, time.Now().UTC()); err != nil {
			log.Error("enqueue warning", "err", err)
		}
	}
}

// assertGitHubAccess confirms the token works and belongs to the configured
// account before any polling starts. Scopes are not checked; fine-grained
// tokens do not report them reliably.
func assertGitHubAccess(ctx context.Context, client *gh.Client, cfg *config.Config, log *slog.Logger) error {
	limits, err := client.RateLimit(ctx)
	if err != nil {
		return fmt.Errorf("github rate limit check failed, is DIBS_GITHUB_TOKEN valid: %w", err)
	}
	user, err := client.User(ctx)
	if err != nil {
		return fmt.Errorf("github identity check failed: %w", err)
	}
	if user.Login != cfg.Profile.GitHubLogin {
		return fmt.Errorf("token belongs to %q but profile.github_login is %q; "+
			"the filter would misread your own comments",
			user.Login, cfg.Profile.GitHubLogin)
	}
	log.Info("github ready",
		"login", user.Login,
		"core_remaining", limits.Resources.Core.Remaining,
		"core_limit", limits.Resources.Core.Limit)
	return nil
}

// watchReload re-reads both config files on SIGHUP and writes the result to the
// database. The poller supervisor notices the change on its own, so adding or
// removing a repository needs no restart.
func watchReload(ctx context.Context, l *loaded, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		}
		repos, err := config.LoadRepos(l.paths.Repos)
		if err != nil {
			log.Error("reload repos, keeping the previous set", "err", err)
			continue
		}
		if err := l.store.SyncRepos(ctx, repos, l.cfg.Polling.DefaultIntervalSec); err != nil {
			log.Error("sync repos after reload", "err", err)
			continue
		}
		log.Info("reloaded repos", "count", len(repos))
	}
}
