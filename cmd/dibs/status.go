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
	today := fs.Bool("today", false, "everything scored in the last 24 hours")
	missed := fs.Bool("missed", false, "issues that scored below the floor in the last 24 hours")
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
		return reportScored(ctx, l, false)
	case *missed:
		return reportScored(ctx, l, true)
	}
	return errors.New("usage: dibs status <--repos|--today|--missed>")
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

// reportScored lists the last day's scored issues. With onlyMissed it shows
// just the ones the floor killed, which is the list M4 reads to decide whether
// the floor is set right.
func reportScored(ctx context.Context, l *loaded, onlyMissed bool) error {
	since := time.Now().UTC().Add(-24 * time.Hour)
	issues, err := l.store.ScoredSince(ctx, since)
	if err != nil {
		return err
	}
	repos := map[int64]store.Repo{}
	shown := 0
	for _, iss := range issues {
		if onlyMissed && iss.RejectReason != store.ReasonBelowFloor {
			continue
		}
		repo, ok := repos[iss.RepoID]
		if !ok {
			if repo, err = l.store.RepoByID(ctx, iss.RepoID); err != nil {
				return err
			}
			repos[iss.RepoID] = repo
		}
		detail := string(iss.State)
		if iss.RejectReason != "" {
			detail = string(iss.RejectReason)
		}
		fmt.Printf("%3d  %-24s #%-6d %-18s %s\n",
			iss.Score.Int64, repo.Slug(), iss.Number, detail, iss.Title)
		shown++
	}
	if shown == 0 {
		fmt.Println("nothing in the last 24 hours")
	}
	return nil
}

// formatCounts prints the states in pipeline order so a report reads the same
// way every time.
func formatCounts(counts map[store.State]int) string {
	order := []store.State{
		store.StateBaseline, store.StateNew, store.StateEnriched, store.StateScored,
		store.StatePushed, store.StateTracked, store.StateSkipped, store.StateSnoozed,
		store.StateRejected, store.StateAgedOut, store.StateClaimedBeforePush, store.StateExpired,
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
