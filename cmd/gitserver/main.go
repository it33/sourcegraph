// gitserver is the gitserver server.
package main // import "github.com/sourcegraph/sourcegraph/cmd/gitserver"

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/inconshreveable/log15"
	jsoniter "github.com/json-iterator/go"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sourcegraph/sourcegraph/cmd/gitserver/server"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/authz"
	codeinteldbstore "github.com/sourcegraph/sourcegraph/internal/codeintel/stores/dbstore"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/database/dbconn"
	"github.com/sourcegraph/sourcegraph/internal/database/dbutil"
	"github.com/sourcegraph/sourcegraph/internal/debugserver"
	"github.com/sourcegraph/sourcegraph/internal/encryption/keyring"
	"github.com/sourcegraph/sourcegraph/internal/env"
	"github.com/sourcegraph/sourcegraph/internal/extsvc"
	"github.com/sourcegraph/sourcegraph/internal/hostname"
	"github.com/sourcegraph/sourcegraph/internal/jsonc"
	"github.com/sourcegraph/sourcegraph/internal/logging"
	"github.com/sourcegraph/sourcegraph/internal/observation"
	"github.com/sourcegraph/sourcegraph/internal/profiler"
	"github.com/sourcegraph/sourcegraph/internal/repos"
	"github.com/sourcegraph/sourcegraph/internal/sentry"
	"github.com/sourcegraph/sourcegraph/internal/trace"
	"github.com/sourcegraph/sourcegraph/internal/trace/ot"
	"github.com/sourcegraph/sourcegraph/internal/tracer"
	"github.com/sourcegraph/sourcegraph/schema"
)

var (
	reposDir                     = env.Get("SRC_REPOS_DIR", "/data/repos", "Root dir containing repos.")
	wantPctFree                  = env.MustGetInt("SRC_REPOS_DESIRED_PERCENT_FREE", 10, "Target percentage of free space on disk.")
	janitorInterval              = env.MustGetDuration("SRC_REPOS_JANITOR_INTERVAL", 1*time.Minute, "Interval between cleanup runs")
	syncRepoStateInterval        = env.MustGetDuration("SRC_REPOS_SYNC_STATE_INTERVAL", 10*time.Minute, "Interval between state syncs")
	syncRepoStateBatchSize       = env.MustGetInt("SRC_REPOS_SYNC_STATE_BATCH_SIZE", 500, "Number of upserts to perform per batch")
	syncRepoStateUpsertPerSecond = env.MustGetInt("SRC_REPOS_SYNC_STATE_UPSERT_PER_SEC", 500, "The number of upserted rows allowed per second across all gitserver instances")
)

func main() {
	ctx := context.Background()

	env.Lock()
	env.HandleHelpFlag()

	if err := profiler.Init(); err != nil {
		log.Fatalf("failed to start profiler: %v", err)
	}

	conf.Init()
	logging.Init()
	tracer.Init()
	sentry.Init()
	trace.Init()

	if reposDir == "" {
		log.Fatal("git-server: SRC_REPOS_DIR is required")
	}
	if err := os.MkdirAll(reposDir, os.ModePerm); err != nil {
		log.Fatalf("failed to create SRC_REPOS_DIR: %s", err)
	}

	wantPctFree2, err := getPercent(wantPctFree)
	if err != nil {
		log.Fatalf("SRC_REPOS_DESIRED_PERCENT_FREE is out of range: %v", err)
	}

	db, err := getDB()
	if err != nil {
		log.Fatalf("failed to initialize database stores: %v", err)
	}

	repoStore := database.Repos(db)
	codeintelDB := codeinteldbstore.NewWithDB(db, &observation.Context{
		Logger:     log15.Root(),
		Tracer:     &trace.Tracer{Tracer: opentracing.GlobalTracer()},
		Registerer: prometheus.DefaultRegisterer,
	}, nil)
	externalServiceStore := database.ExternalServices(db)

	err = keyring.Init(ctx)
	if err != nil {
		log.Fatalf("failed to initialise keyring: %s", err)
	}

	gitserver := server.Server{
		ReposDir:           reposDir,
		DesiredPercentFree: wantPctFree2,
		GetRemoteURLFunc: func(ctx context.Context, repo api.RepoName) (string, error) {
			r, err := repoStore.GetByName(ctx, repo)
			if err != nil {
				return "", err
			}

			for _, info := range r.Sources {
				// build the clone url using the external service config instead of using
				// the source CloneURL field
				svc, err := externalServiceStore.GetByID(ctx, info.ExternalServiceID())
				if err != nil {
					return "", err
				}

				return repos.CloneURL(svc.Kind, svc.Config, r)
			}
			return "", errors.Errorf("no sources for %q", repo)
		},
		GetVCSSyncer: func(ctx context.Context, repo api.RepoName) (server.VCSSyncer, error) {
			// We need an internal actor in case we are trying to access a private repo. We
			// only need access in order to find out the type of code host we're using, so
			// it's safe.
			r, err := repoStore.GetByName(actor.WithInternalActor(ctx), repo)
			if err != nil {
				return nil, errors.Wrap(err, "get repository")
			}

			switch r.ExternalRepo.ServiceType {
			case extsvc.TypePerforce:
				// Extract options from external service config
				var c schema.PerforceConnection
				for _, info := range r.Sources {
					es, err := externalServiceStore.GetByID(ctx, info.ExternalServiceID())
					if err != nil {
						return nil, errors.Wrap(err, "get external service")
					}

					normalized, err := jsonc.Parse(es.Config)
					if err != nil {
						return nil, errors.Wrap(err, "normalize JSON")
					}

					if err = jsoniter.Unmarshal(normalized, &c); err != nil {
						return nil, errors.Wrap(err, "unmarshal JSON")
					}
					break
				}

				return &server.PerforceDepotSyncer{
					MaxChanges: int(c.MaxChanges),
					Client:     c.P4Client,
				}, nil
			case extsvc.TypeJVMPackages:
				var c schema.JVMPackagesConnection
				for _, info := range r.Sources {
					es, err := externalServiceStore.GetByID(ctx, info.ExternalServiceID())
					if err != nil {
						return nil, errors.Wrap(err, "get external service")
					}

					normalized, err := jsonc.Parse(es.Config)
					if err != nil {
						return nil, errors.Wrap(err, "normalize JSON")
					}

					if err = jsoniter.Unmarshal(normalized, &c); err != nil {
						return nil, errors.Wrap(err, "unmarshal JSON")
					}
					break
				}

				return &server.JVMPackagesSyncer{Config: &c, DBStore: codeintelDB}, nil
			}
			return &server.GitRepoSyncer{}, nil
		},
		Hostname: hostname.Get(),
		DB:       db,
	}
	gitserver.RegisterMetrics()

	if tmpDir, err := gitserver.SetupAndClearTmp(); err != nil {
		log.Fatalf("failed to setup temporary directory: %s", err)
	} else if err := os.Setenv("TMP_DIR", tmpDir); err != nil {
		// Additionally, set TMP_DIR so other temporary files we may accidentally
		// create are on the faster RepoDir mount.
		log.Fatalf("Setting TMP_DIR: %s", err)
	}

	// Create Handler now since it also initializes state

	// TODO: Why do we set server state as a side effect of creating our handler?
	handler := ot.Middleware(trace.HTTPTraceMiddleware(gitserver.Handler()))

	// Ready immediately
	ready := make(chan struct{})
	close(ready)
	go debugserver.NewServerRoutine(ready).Start()
	go gitserver.Janitor(janitorInterval)
	go gitserver.SyncRepoState(syncRepoStateInterval, syncRepoStateBatchSize, syncRepoStateUpsertPerSecond)

	port := "3178"
	host := ""
	if env.InsecureDev {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, port)
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}
	log15.Info("git-server: listening", "addr", srv.Addr)

	go func() {
		err := srv.ListenAndServe()
		if err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// Listen for shutdown signals. When we receive one attempt to clean up,
	// but do an insta-shutdown if we receive more than one signal.
	c := make(chan os.Signal, 2)
	signal.Notify(c, syscall.SIGINT, syscall.SIGHUP)
	<-c
	go func() {
		<-c
		os.Exit(0)
	}()

	// Stop accepting requests. In the future we should use graceful shutdown.
	if err := srv.Close(); err != nil {
		log15.Error("closing http server", "error", err)
	}

	// The most important thing this does is kill all our clones. If we just
	// shutdown they will be orphaned and continue running.
	gitserver.Stop()
}

func getPercent(p int) (int, error) {
	if p < 0 {
		return 0, errors.Errorf("negative value given for percentage: %d", p)
	}
	if p > 100 {
		return 0, errors.Errorf("excessively high value given for percentage: %d", p)
	}
	return p, nil
}

// getStores initializes a connection to the database and returns RepoStore and
// ExternalServiceStore.
func getDB() (dbutil.DB, error) {

	//
	// START FLAILING

	// Gitserver is an internal actor. We rely on the frontend to do authz checks for
	// user requests.
	//
	// This call to SetProviders is here so that calls to GetProviders don't block.
	authz.SetProviders(true, []authz.Provider{})

	// END FLAILING
	//

	dsn := conf.Get().ServiceConnections.PostgresDSN
	conf.Watch(func() {
		newDSN := conf.Get().ServiceConnections.PostgresDSN
		if dsn != newDSN {
			// The DSN was changed (e.g. by someone modifying the env vars on
			// the frontend). We need to respect the new DSN. Easiest way to do
			// that is to restart our service (kubernetes/docker/goreman will
			// handle starting us back up).
			log.Fatalf("Detected repository DSN change, restarting to take effect: %q", newDSN)
		}
	})

	return dbconn.New(dbconn.Opts{DSN: dsn, DBName: "frontend", AppName: "gitserver"})
}
