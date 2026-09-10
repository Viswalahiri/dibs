package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/Viswalahiri/dibs/internal/store"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	repos := fs.Bool("repos", false, "per-repository counts by state")
	today := fs.Bool("today", false, "everything notified in the last 24 hours")
	if err := fs.Parse(args); err != nil {
		return err
	}

	l, err := load()
	if err != nil {
		return err
	}
	defer l.store.Close()
	ctx := context.Background()

	switch {
	case *repos:
		return reportRepos(ctx, l)
	case *today:
		return reportPushed(ctx, l)
	}
	return errors.New("usage: dibs status <--repos|--today>")
}

func reportRepos(ctx context.Context, l *loaded) error {
	all, err := l.store.EnabledRepos(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		// repos.yaml is read into the database by `dibs run`, so a status call
		// before the first run has nothing to report and should say so rather
		// than printing an empty page.
		fmt.Printf("no repositories in the database yet; %d are configured in %s.\n",
			len(l.repos), l.paths.Repos)
		fmt.Println("run `dibs run` once to adopt them.")
		return nil
	}
	for _, repo := range all {
		counts, err := l.store.CountByState(ctx, repo.ID)
		if err != nil {
			return err
		}
		adopted := "not adopted"
		if repo.Adopted() {
			adopted = "waterline " + repo.WaterlineAt.Format(time.RFC3339)
		}
		fmt.Printf("%-32s %-38s %s\n", repo.Slug(), adopted, formatCounts(counts))
	}
	return nil
}

// reportPushed lists the last day's notifications, newest first. It is the
// answer to "what did dibs send me while I was away".
func reportPushed(ctx context.Context, l *loaded) error {
	issues, err := l.store.PushedSince(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		return err
	}
	repos := map[int64]store.Repo{}
	for _, iss := range issues {
		repo, ok := repos[iss.RepoID]
		if !ok {
			if repo, err = l.store.RepoByID(ctx, iss.RepoID); err != nil {
				return err
			}
			repos[iss.RepoID] = repo
		}
		fmt.Printf("%s  %-24s #%-6d %s\n",
			iss.SurfacedAt.Format("15:04"), repo.Slug(), iss.Number, iss.Title)
	}
	if len(issues) == 0 {
		fmt.Println("nothing in the last 24 hours")
	}
	return nil
}

// formatCounts prints the states in pipeline order so a report reads the same
// way every time.
func formatCounts(counts map[store.State]int) string {
	order := []store.State{
		store.StateBaseline, store.StateNew, store.StateReady, store.StatePushed,
		store.StateRejected, store.StateAgedOut, store.StateClaimedBeforePush,
		store.StateExpired,
	}
	out := ""
	for _, st := range order {
		if n := counts[st]; n > 0 {
			if out != "" {
				out += " "
			}
			out += fmt.Sprintf("%s=%d", st, n)
		}
	}
	if out == "" {
		return "no issues"
	}
	return out
}
