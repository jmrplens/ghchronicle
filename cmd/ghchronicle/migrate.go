package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/migrate"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// migratePlan prints, for every configured store, what an earlier release
// left there in a shape this one no longer writes and what bringing it along
// would take.
//
// It changes nothing, anywhere: the stores are asked questions, GitHub is
// asked for the repository list, and the state file is read and never
// written, not even to record what the stores said. A dry run that wrote
// its findings down would make the next plan read differently from this one
// for no reason the reader could see.
func migratePlan(ctx context.Context, cfg *config.Config, api *ghapi.Client, confirmed bool,
	stdout io.Writer, now time.Time,
) error {
	if confirmed {
		return errors.New("-migrate -yes is not in this build: -migrate alone prints the plan and changes nothing")
	}
	in := migrate.Input{
		Config: cfg, State: run.LoadState(cfg.StateFile), Release: version, Now: now,
	}
	in.Repos, in.ReposWhy = coveredRepos(ctx, api, cfg)
	in.ReposKnown = in.ReposWhy == ""
	migrate.Make(ctx, in).Print(stdout)
	return nil
}

// coveredRepos is every repository a refill of cfg would read, which is what
// a backfill covers, archived repositories included, or why the list could
// not be read. Without a token, or with GitHub not answering, the plan still
// runs and says what it could not compare.
func coveredRepos(ctx context.Context, api *ghapi.Client, cfg *config.Config) (repos []string, why string) {
	if cfg.GitHub.Token == "" {
		return nil, "the configuration has no GitHub token"
	}
	found, err := collect.Discover(ctx, api, run.DiscoveryFilter(cfg, true))
	if err != nil {
		return nil, err.Error()
	}
	repos = make([]string, 0, len(found.Repos))
	for _, r := range found.Repos {
		repos = append(repos, r.FullName)
	}
	return repos, ""
}
