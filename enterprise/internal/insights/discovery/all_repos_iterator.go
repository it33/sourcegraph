package discovery

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sourcegraph/sourcegraph/internal/actor"

	"github.com/cockroachdb/errors"

	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/types"
)

// IndexableReposLister is a subset of the API exposed by the backend.ListIndexable.
type IndexableReposLister interface {
	List(ctx context.Context) ([]types.RepoName, error)
}

// RepoStore is a subset of the API exposed by the database.Repos() store.
type RepoStore interface {
	List(ctx context.Context, opt database.ReposListOptions) (results []*types.Repo, err error)
}

// AllReposIterator implements an efficient way to iterate over every single repository on
// Sourcegraph that should be considered for code insights.
//
// It caches multiple consecutive uses in order to ensure repository lists (which can be quite
// large, e.g. 500,000+ repositories) are only fetched as frequently as needed.
type AllReposIterator struct {
	IndexableReposLister  IndexableReposLister
	RepoStore             RepoStore
	Clock                 func() time.Time
	SourcegraphDotComMode bool // result of envvar.SourcegraphDotComMode()

	// RepositoryListCacheTime describes how long to cache repository lists for. These API calls
	// can result in hundreds of thousands of repositories, so choose wisely as it can be expensive
	// to pull such large numbers of rows from the DB frequently.
	RepositoryListCacheTime time.Duration

	counter *prometheus.CounterVec

	// Internal fields below.
	cachedRepoNamesAge time.Time
	cachedRepoNames    []string
	cachedPageRequests map[database.LimitOffset]cachedPageRequest
}

func NewAllReposIterator(indexableReposLister IndexableReposLister, repoStore RepoStore, clock func() time.Time, sourcegraphDotComMode bool, repositoryListCacheTime time.Duration, counterOpts *prometheus.CounterOpts) *AllReposIterator {
	return &AllReposIterator{IndexableReposLister: indexableReposLister, RepoStore: repoStore, Clock: clock, SourcegraphDotComMode: sourcegraphDotComMode, RepositoryListCacheTime: repositoryListCacheTime, counter: promauto.NewCounterVec(*counterOpts, []string{"result"})}
}

func (a *AllReposIterator) timeSince(t time.Time) time.Duration {
	return a.Clock().Sub(t)
}

// ForEach invokes the given function for every repository that we should consider gathering data
// for historically.
//
// This takes into account paginating repository names from the database (as there could be e.g.
// 500,000+ of them). It also takes into account Sourcegraph.com, where we only gather historical
// data for the same subset of repos we index for search.
//
// If the forEach function returns an error, pagination is stopped and the error returned.
func (a *AllReposIterator) ForEach(ctx context.Context, forEach func(repoName string) error) error {
	// 🚨 SECURITY: this context will ensure that this iterator goes over all repositories
	globalCtx := actor.WithInternalActor(ctx)

	if a.SourcegraphDotComMode {
		// Has the cache expired or empty? If so, refresh it.
		if a.timeSince(a.cachedRepoNamesAge) > a.RepositoryListCacheTime || a.cachedRepoNames == nil {
			a.cachedRepoNames = a.cachedRepoNames[:0]

			// We shouldn't try to fill historical data for ALL repos on Sourcegraph.com, it would take
			// forever. Instead, we use the same list of indexable repositories used when you do a global
			// search on Sourcegraph.com.
			res, err := a.IndexableReposLister.List(globalCtx)
			if err != nil {
				return errors.Wrap(err, "IndexableReposLister.List")
			}
			for _, r := range res {
				a.cachedRepoNames = append(a.cachedRepoNames, string(r.Name))
			}
			a.cachedRepoNamesAge = a.Clock()
		}
		for _, repo := range a.cachedRepoNames {
			if err := forEach(repo); err != nil {
				a.counter.WithLabelValues("error").Inc()
				return errors.Wrap(err, "forEach")
			}
			a.counter.WithLabelValues("success").Inc()
		}
		return nil
	}

	// Regular deployments of Sourcegraph.
	//
	// We paginate 1,000 repositories out of the DB at a time.
	limitOffset := database.LimitOffset{
		Limit:  1000,
		Offset: 0,
	}
	for {
		// Get the next page.
		repos, err := a.cachedRepoStoreList(ctx, limitOffset)
		if err != nil {
			return errors.Wrap(err, "RepoStore.List")
		}
		if len(repos) == 0 {
			return nil // done!
		}

		// Call the forEach function on every repository.
		for _, r := range repos {
			if err := forEach(string(r.Name)); err != nil {
				a.counter.WithLabelValues("error").Inc()
				return errors.Wrap(err, "forEach")
			}
			a.counter.WithLabelValues("success").Inc()

		}

		// Set outselves up to get the next page.
		limitOffset.Offset += len(repos)
	}
}

// cachedRepoStoreList calls a.repoStore.List to do a paginated list of repositories, and caches the
// results in-memory for some time.
//
// This is primarily useful because we call this function e.g. 1 time per 365 days.
func (a *AllReposIterator) cachedRepoStoreList(ctx context.Context, page database.LimitOffset) ([]*types.Repo, error) {
	if a.cachedPageRequests == nil {
		a.cachedPageRequests = map[database.LimitOffset]cachedPageRequest{}
	}
	cacheEntry, ok := a.cachedPageRequests[page]
	if ok && a.timeSince(cacheEntry.age) < a.RepositoryListCacheTime {
		return cacheEntry.results, nil
	}

	trueP := true
	repos, err := a.RepoStore.List(ctx, database.ReposListOptions{
		Index: &trueP,

		// Order by repository name.
		OrderBy: database.RepoListOrderBy{{Field: database.RepoListName}},

		LimitOffset: &page,
	})
	if err != nil {
		return nil, err
	}
	a.cachedPageRequests[page] = cachedPageRequest{
		age:     a.Clock(),
		results: repos,
	}
	return repos, nil
}

type cachedPageRequest struct {
	age     time.Time
	results []*types.Repo
}
