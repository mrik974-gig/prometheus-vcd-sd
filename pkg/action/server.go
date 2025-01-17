package action

import (
	"context"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/promhippie/prometheus-vcd-sd/pkg/adapter"
	"github.com/promhippie/prometheus-vcd-sd/pkg/client"
	"github.com/promhippie/prometheus-vcd-sd/pkg/config"
	"github.com/promhippie/prometheus-vcd-sd/pkg/middleware"
	"github.com/promhippie/prometheus-vcd-sd/pkg/version"
)

// Server handles the server sub-command.
func Server(cfg *config.Config, logger log.Logger) error {
	level.Info(logger).Log(
		"msg", "Launching Prometheus vCloud Director SD",
		"version", version.String,
		"revision", version.Revision,
		"date", version.Date,
		"go", version.Go,
		"engine", cfg.Target.Engine,
	)

	var gr run.Group

	{
		ctx := context.Background()
		configs := make(map[string]*client.Client, len(cfg.Target.Credentials))

		for _, credential := range cfg.Target.Credentials {
			parsed, err := url.ParseRequestURI(credential.URL)

			if err != nil {
				level.Error(logger).Log(
					"msg", ErrClientEndpoint,
					"project", credential.Project,
				)

				return ErrClientEndpoint
			}

			configs[credential.Project] = client.New(
				parsed,
				credential.Insecure,
				credential.Username,
				credential.Password,
				credential.Org,
				credential.Vdc,
			)
		}

		disc := Discoverer{
			configs:   configs,
			logger:    logger,
			refresh:   cfg.Target.Refresh,
			separator: ",",
			lasts:     make(map[string]struct{}),
		}

		a := adapter.NewAdapter(ctx, cfg.Target.File, "vcd-sd", disc, logger)
		a.Run()
	}

	{
		server := &http.Server{
			Addr:         cfg.Server.Addr,
			Handler:      handler(cfg, logger),
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
		}

		gr.Add(func() error {
			level.Info(logger).Log(
				"msg", "Starting metrics server",
				"addr", cfg.Server.Addr,
			)

			return web.ListenAndServe(
				server,
				&web.FlagConfig{
					WebListenAddresses: sliceP([]string{cfg.Server.Addr}),
					WebSystemdSocket:   boolP(false),
					WebConfigFile:      stringP(cfg.Server.Web),
				},
				logger,
			)
		}, func(reason error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if err := server.Shutdown(ctx); err != nil {
				level.Error(logger).Log(
					"msg", "Failed to shutdown metrics gracefully",
					"err", err,
				)

				return
			}

			level.Info(logger).Log(
				"msg", "Metrics shutdown gracefully",
				"reason", reason,
			)
		})
	}

	{
		stop := make(chan os.Signal, 1)

		gr.Add(func() error {
			signal.Notify(stop, os.Interrupt)

			<-stop

			return nil
		}, func(err error) {
			close(stop)
		})
	}

	return gr.Run()
}

func handler(cfg *config.Config, logger log.Logger) *chi.Mux {
	mux := chi.NewRouter()
	mux.Use(middleware.Recoverer(logger))
	mux.Use(middleware.RealIP)
	mux.Use(middleware.Timeout)
	mux.Use(middleware.Cache)

	reg := promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{
			ErrorLog: promLogger{logger},
		},
	)

	mux.Route("/", func(root chi.Router) {
		root.Get(cfg.Server.Path, func(w http.ResponseWriter, r *http.Request) {
			reg.ServeHTTP(w, r)
		})

		root.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)

			io.WriteString(w, http.StatusText(http.StatusOK))
		})

		root.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)

			io.WriteString(w, http.StatusText(http.StatusOK))
		})

		if cfg.Target.Engine == "http" {
			root.Get("/sd", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")

				content, err := ioutil.ReadFile(cfg.Target.File)

				if err != nil {
					level.Info(logger).Log(
						"msg", "Failed to read service discovery data",
						"err", err,
					)

					http.Error(
						w,
						"Failed to read service discovery data",
						http.StatusInternalServerError,
					)

					return
				}

				w.WriteHeader(http.StatusOK)
				w.Write(content)
			})
		}
	})

	return mux
}

func boolP(i bool) *bool {
	return &i
}

func stringP(i string) *string {
	return &i
}

func sliceP(i []string) *[]string {
	return &i
}
