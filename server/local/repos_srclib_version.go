package local

import (
	"fmt"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"gopkg.in/inconshreveable/log15.v2"
	srclibstore "sourcegraph.com/sourcegraph/srclib/store"
	"src.sourcegraph.com/sourcegraph/errcode"
	"src.sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"src.sourcegraph.com/sourcegraph/pkg/vcs"
	"src.sourcegraph.com/sourcegraph/server/accesscontrol"
	localcli "src.sourcegraph.com/sourcegraph/server/local/cli"
	"src.sourcegraph.com/sourcegraph/store"
	"src.sourcegraph.com/sourcegraph/svc"
)

func (s *repos) GetSrclibDataVersionForPath(ctx context.Context, entry *sourcegraph.TreeEntrySpec) (*sourcegraph.SrclibDataVersion, error) {
	if err := accesscontrol.VerifyUserHasReadAccess(ctx, "Repos.GetSrclibDataVersionForPath", entry.RepoRev.URI); err != nil {
		return nil, err
	}

	if err := s.resolveRepoRev(ctx, &entry.RepoRev); err != nil {
		return nil, err
	}

	// First, try to find an exact match.
	vers, err := store.GraphFromContext(ctx).Versions(
		srclibstore.ByRepoCommitIDs(srclibstore.Version{Repo: entry.RepoRev.URI, CommitID: entry.RepoRev.CommitID}),
	)
	if err != nil {
		return nil, err
	}
	if len(vers) == 1 {
		log15.Debug("svc.local.repos.GetSrclibDataVersionForPath", "entry", entry, "result", "exact match")
		return &sourcegraph.SrclibDataVersion{CommitID: vers[0].CommitID, CommitsBehind: 0}, nil
	}

	if entry.Path == "." {
		// All commits affect the root, so there is no hope of finding
		// an earlier srclib-built commit that we can use.
		log15.Debug("svc.local.repos.GetSrclibDataVersionForPath", "entry", entry, "result", "no version for root")
		return nil, grpc.Errorf(codes.NotFound, "no srclib data version found for head commit %v (can't look-back because path is root)", entry.RepoRev)
	}

	// Do expensive search backwards through history.
	info, err := s.getSrclibDataVersionForPathLookback(ctx, entry)
	if err != nil {
		if errcode.GRPC(err) == codes.NotFound {
			log15.Debug("svc.local.repos.GetSrclibDataVersionForPath", "entry", entry, "result", "not found: "+err.Error())
		}
		return nil, err
	}
	log15.Debug("svc.local.repos.GetSrclibDataVersionForPath", "entry", entry, "result", fmt.Sprintf("lookback match %+v", info))
	return info, nil
}

func (s *repos) getSrclibDataVersionForPathLookback(ctx context.Context, entry *sourcegraph.TreeEntrySpec) (*sourcegraph.SrclibDataVersion, error) {
	// Find the base commit (the farthest ancestor commit we'll
	// consider).
	//
	// If entry.Path is empty, we theoretically are OK going back as
	// far as possible. This is the intended behavior for repo-wide
	// actions (such as search), where there is no non-arbitrary point
	// to stop our lookback. However, we apply a lookback limit for
	// performance reasons.
	//
	// If entry.Path is set, then we need to find a commit equal to or
	// a descendant of the last commit that touched that
	// path. Otherwise, we'd return srclib data that applies to a
	// different version of the file.
	var base string
	if entry.Path != "" {
		lastPathCommit, err := svc.Repos(ctx).ListCommits(ctx, &sourcegraph.ReposListCommitsOp{
			Repo: entry.RepoRev.RepoSpec,
			Opt: &sourcegraph.RepoListCommitsOptions{
				Head:        entry.RepoRev.CommitID,
				Path:        entry.Path,
				ListOptions: sourcegraph.ListOptions{PerPage: 1}, // only the most recent commit needed
			},
		})
		if err != nil {
			return nil, err
		}
		if len(lastPathCommit.Commits) != 1 {
			return nil, grpc.Errorf(codes.NotFound, "no commits found for path %q in repo %v", entry.Path, entry.RepoRev)
		}
		lastPathCommitID := string(lastPathCommit.Commits[0].ID)
		if entry.RepoRev.CommitID == lastPathCommitID {
			// We have already looked checked if we have a build
			// for entry.RepoRev.CommitID, so there is no hope to
			// finding an earlier srclib-built commit that we can
			// use.
			return nil, grpc.Errorf(codes.NotFound, "no srclib data version found for head commit %v (can't look-back because path  was last modified by head commit)", entry.RepoRev)

		}
		base = lastPathCommitID
	}

	// TODO(beyang): move clcache flag into lookbackLimit flag
	var lookbackLimit int32 = 250
	if localcli.Flags.CommitLogCacheSize > 250 {
		lookbackLimit = localcli.Flags.CommitLogCacheSize
	}

	// List the recent commits that we'll use to check for builds.
	candidateCommits, err := svc.Repos(ctx).ListCommits(ctx,
		&sourcegraph.ReposListCommitsOp{
			Repo: entry.RepoRev.RepoSpec,
			Opt: &sourcegraph.RepoListCommitsOptions{
				Head: entry.RepoRev.CommitID,
				Base: base,
				ListOptions: sourcegraph.ListOptions{
					// Note: if cached, lookback is limited to the
					// number of commits in the commit log that are
					// cached.
					PerPage: lookbackLimit,
				},
			},
		},
	)
	if err != nil {
		return nil, err
	}

	// If we had a base, then the candidateCommits list will exclude
	// the base (by the specification of git ranges; see `man
	// gitrevision`). But we want to include the base commit when
	// searching for versions, so add it back.
	if base != "" {
		candidateCommits.Commits = append(candidateCommits.Commits, &vcs.Commit{ID: vcs.CommitID(base)})
	}

	candidateCommitIDs := make([]string, len(candidateCommits.Commits))
	for i, c := range candidateCommits.Commits {
		candidateCommitIDs[i] = string(c.ID)
	}

	// Get all srclib built data versions.
	vers, err := store.GraphFromContext(ctx).Versions(
		srclibstore.ByRepos(entry.RepoRev.URI),
		srclibstore.ByCommitIDs(candidateCommitIDs...),
	)
	if err != nil {
		return nil, err
	}
	verMap := make(map[string]struct{}, len(vers))
	for _, ver := range vers {
		verMap[ver.CommitID] = struct{}{}
	}

	for i, cc := range candidateCommitIDs {
		if _, present := verMap[cc]; present {
			return &sourcegraph.SrclibDataVersion{CommitID: cc, CommitsBehind: int32(i)}, nil
		}
	}

	return nil, grpc.Errorf(codes.NotFound, "no srclib data versions found for %v (%d candidate commits, %d srclib data versions)", entry, len(candidateCommits.Commits), len(vers))
}
