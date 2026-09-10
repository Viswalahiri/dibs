package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/triage"
)

// cmdReplay re-scores stored model responses under a candidate config. It makes
// no API calls, which is what makes the junk floor and the veto threshold
// tunable at all.
func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	candidate := fs.String("config", "", "candidate config to score against; defaults to the live one")
	verbose := fs.Bool("v", false, "list every issue whose verdict changes")
	if err := fs.Parse(args); err != nil {
		return err
	}

	l, err := load()
	if err != nil {
		return err
	}
	defer l.store.Close()

	cfg := l.cfg
	if *candidate != "" {
		if cfg, err = config.Load(*candidate); err != nil {
			return err
		}
	}

	rows, summary, err := triage.Replay(context.Background(), l.store, cfg)
	if err != nil {
		return err
	}
	if *verbose {
		for _, r := range rows {
			if !r.Changed() {
				continue
			}
			verdict := "now rejected: " + string(r.NewReason)
			if r.NewPush {
				verdict = "now pushed"
			}
			fmt.Printf("%3d -> %3d  %-24s #%-6d %-14s %s\n  %s\n",
				r.StoredScore, r.NewScore, r.Repo, r.Number, verdict, r.Title, r.URL)
		}
		fmt.Println()
	}
	fmt.Println(summary)
	return nil
}
