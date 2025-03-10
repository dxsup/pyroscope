package phlare

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/bufbuild/connect-go"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gorilla/mux"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/grpcutil"
	"github.com/grafana/dskit/kv/memberlist"
	dslog "github.com/grafana/dskit/log"
	"github.com/grafana/dskit/modules"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/runtimeconfig"
	"github.com/grafana/dskit/server"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/signals"
	wwtracing "github.com/grafana/dskit/tracing"
	"github.com/grafana/pyroscope-go"
	grpcgw "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"github.com/samber/lo"

	"github.com/grafana/pyroscope/pkg/api"
	"github.com/grafana/pyroscope/pkg/cfg"
	"github.com/grafana/pyroscope/pkg/compactor"
	"github.com/grafana/pyroscope/pkg/distributor"
	"github.com/grafana/pyroscope/pkg/frontend"
	"github.com/grafana/pyroscope/pkg/ingester"
	phlareobj "github.com/grafana/pyroscope/pkg/objstore"
	objstoreclient "github.com/grafana/pyroscope/pkg/objstore/client"
	phlarecontext "github.com/grafana/pyroscope/pkg/phlare/context"
	"github.com/grafana/pyroscope/pkg/phlaredb"
	"github.com/grafana/pyroscope/pkg/querier"
	"github.com/grafana/pyroscope/pkg/querier/worker"
	"github.com/grafana/pyroscope/pkg/scheduler"
	"github.com/grafana/pyroscope/pkg/scheduler/schedulerdiscovery"
	"github.com/grafana/pyroscope/pkg/storegateway"
	"github.com/grafana/pyroscope/pkg/tenant"
	"github.com/grafana/pyroscope/pkg/tracing"
	"github.com/grafana/pyroscope/pkg/usagestats"
	"github.com/grafana/pyroscope/pkg/util"
	"github.com/grafana/pyroscope/pkg/util/cli"
	"github.com/grafana/pyroscope/pkg/util/spanprofiler"
	"github.com/grafana/pyroscope/pkg/validation"
	"github.com/grafana/pyroscope/pkg/validation/exporter"
)

type Config struct {
	Target            flagext.StringSliceCSV `yaml:"target,omitempty"`
	API               api.Config             `yaml:"api"`
	Server            server.Config          `yaml:"server,omitempty"`
	Distributor       distributor.Config     `yaml:"distributor,omitempty"`
	Querier           querier.Config         `yaml:"querier,omitempty"`
	Frontend          frontend.Config        `yaml:"frontend,omitempty"`
	Worker            worker.Config          `yaml:"frontend_worker"`
	LimitsConfig      validation.Limits      `yaml:"limits"`
	QueryScheduler    scheduler.Config       `yaml:"query_scheduler"`
	Ingester          ingester.Config        `yaml:"ingester,omitempty"`
	StoreGateway      storegateway.Config    `yaml:"store_gateway,omitempty"`
	MemberlistKV      memberlist.KVConfig    `yaml:"memberlist"`
	PhlareDB          phlaredb.Config        `yaml:"pyroscopedb,omitempty"`
	Tracing           tracing.Config         `yaml:"tracing"`
	OverridesExporter exporter.Config        `yaml:"overrides_exporter" doc:"hidden"`
	RuntimeConfig     runtimeconfig.Config   `yaml:"runtime_config"`
	Compactor         compactor.Config       `yaml:"compactor"`

	Storage       StorageConfig       `yaml:"storage"`
	SelfProfiling SelfProfilingConfig `yaml:"self_profiling,omitempty"`

	MultitenancyEnabled bool              `yaml:"multitenancy_enabled,omitempty"`
	Analytics           usagestats.Config `yaml:"analytics"`

	ConfigFile      string `yaml:"-"`
	ConfigExpandEnv bool   `yaml:"-"`
}

func newDefaultConfig() *Config {
	defaultConfig := &Config{}
	defaultFS := flag.NewFlagSet("", flag.PanicOnError)
	defaultConfig.RegisterFlags(defaultFS)
	return defaultConfig
}

type StorageConfig struct {
	Bucket objstoreclient.Config `yaml:",inline"`
}

func (c *StorageConfig) RegisterFlagsWithContext(ctx context.Context, f *flag.FlagSet) {
	c.Bucket.RegisterFlagsWithPrefix("storage.", f, phlarecontext.Logger(ctx))
}

type SelfProfilingConfig struct {
	DisablePush          bool `yaml:"disable_push,omitempty"`
	MutexProfileFraction int  `yaml:"mutex_profile_fraction,omitempty"`
	BlockProfileRate     int  `yaml:"block_profile_rate,omitempty"`
}

func (c *SelfProfilingConfig) RegisterFlags(f *flag.FlagSet) {
	// these are values that worked well in OG Pyroscope Cloud without adding much overhead
	f.IntVar(&c.MutexProfileFraction, "self-profiling.mutex-profile-fraction", 5, "")
	f.IntVar(&c.BlockProfileRate, "self-profiling.block-profile-rate", 5, "")
	f.BoolVar(&c.DisablePush, "self-profiling.disable-push", false, "When running in single binary (--target=all) Pyroscope will push (Go SDK) profiles to itself. Set to true to disable self-profiling.")
}

func (c *Config) RegisterFlags(f *flag.FlagSet) {
	c.RegisterFlagsWithContext(context.Background(), f)
}

// RegisterFlagsWithContext registers flag.
func (c *Config) RegisterFlagsWithContext(ctx context.Context, f *flag.FlagSet) {
	// Set the default module list to 'all'
	c.Target = []string{All}
	f.StringVar(&c.ConfigFile, "config.file", "", "yaml file to load")
	f.Var(&c.Target, "target", "Comma-separated list of Pyroscope modules to load. "+
		"The alias 'all' can be used in the list to load a number of core modules and will enable single-binary mode. ")
	f.BoolVar(&c.MultitenancyEnabled, "auth.multitenancy-enabled", false, "When set to true, incoming HTTP requests must specify tenant ID in HTTP X-Scope-OrgId header. When set to false, tenant ID anonymous is used instead.")
	f.BoolVar(&c.ConfigExpandEnv, "config.expand-env", false, "Expands ${var} in config according to the values of the environment variables.")

	c.registerServerFlagsWithChangedDefaultValues(f)
	c.MemberlistKV.RegisterFlags(f)
	c.Querier.RegisterFlags(f)
	c.StoreGateway.RegisterFlags(f, util.Logger)
	c.PhlareDB.RegisterFlags(f)
	c.Tracing.RegisterFlags(f)
	c.Storage.RegisterFlagsWithContext(ctx, f)
	c.SelfProfiling.RegisterFlags(f)
	c.RuntimeConfig.RegisterFlags(f)
	c.Analytics.RegisterFlags(f)
	c.LimitsConfig.RegisterFlags(f)
	c.Compactor.RegisterFlags(f, log.NewLogfmtLogger(os.Stderr))
	c.API.RegisterFlags(f)
}

// registerServerFlagsWithChangedDefaultValues registers *Config.Server flags, but overrides some defaults set by the weaveworks package.
func (c *Config) registerServerFlagsWithChangedDefaultValues(fs *flag.FlagSet) {
	throwaway := flag.NewFlagSet("throwaway", flag.PanicOnError)

	// Register to throwaway flags first. Default values are remembered during registration and cannot be changed,
	// but we can take values from throwaway flag set and reregister into supplied flags with new default values.
	c.Server.RegisterFlags(throwaway)
	c.Ingester.RegisterFlags(throwaway)
	c.Distributor.RegisterFlags(throwaway, log.NewLogfmtLogger(os.Stderr))
	c.Frontend.RegisterFlags(throwaway, log.NewLogfmtLogger(os.Stderr))
	c.QueryScheduler.RegisterFlags(throwaway, log.NewLogfmtLogger(os.Stderr))
	c.Worker.RegisterFlags(throwaway)
	c.OverridesExporter.RegisterFlags(throwaway, log.NewLogfmtLogger(os.Stderr))

	throwaway.VisitAll(func(f *flag.Flag) {
		// Ignore errors when setting new values. We have a test to verify that it works.
		switch f.Name {
		case "server.http-listen-port":
			_ = f.Value.Set("4040")
		case "distributor.replication-factor":
			_ = f.Value.Set("1")
		case "query-scheduler.service-discovery-mode":
			_ = f.Value.Set(schedulerdiscovery.ModeRing)
		}
		fs.Var(f.Value, f.Name, f.Usage)
	})
}

func (c *Config) Validate() error {
	if len(c.Target) == 0 {
		return errors.New("no modules specified")
	}
	if err := c.Compactor.Validate(c.PhlareDB.MaxBlockDuration); err != nil {
		return err
	}
	return c.Ingester.Validate()
}

func (c *Config) ApplyDynamicConfig() cfg.Source {
	c.Ingester.LifecyclerConfig.RingConfig.KVStore.Store = "memberlist"
	c.Distributor.DistributorRing.KVStore.Store = c.Ingester.LifecyclerConfig.RingConfig.KVStore.Store
	c.OverridesExporter.Ring.Ring.KVStore.Store = c.Ingester.LifecyclerConfig.RingConfig.KVStore.Store
	c.Frontend.QuerySchedulerDiscovery.SchedulerRing.KVStore.Store = c.Ingester.LifecyclerConfig.RingConfig.KVStore.Store
	c.Worker.QuerySchedulerDiscovery.SchedulerRing.KVStore.Store = c.Ingester.LifecyclerConfig.RingConfig.KVStore.Store
	c.QueryScheduler.ServiceDiscovery.SchedulerRing.KVStore.Store = c.Ingester.LifecyclerConfig.RingConfig.KVStore.Store
	c.StoreGateway.ShardingRing.Ring.KVStore.Store = c.Ingester.LifecyclerConfig.RingConfig.KVStore.Store
	c.Compactor.ShardingRing.Common.KVStore.Store = c.Ingester.LifecyclerConfig.RingConfig.KVStore.Store

	return func(dst cfg.Cloneable) error {
		return nil
	}
}

func (c *Config) Clone() flagext.Registerer {
	return func(c Config) *Config {
		return &c
	}(*c)
}

type Phlare struct {
	Cfg    Config
	logger log.Logger
	reg    prometheus.Registerer
	tracer io.Closer

	ModuleManager *modules.Manager
	serviceMap    map[string]services.Service
	deps          map[string][]string

	API           *api.API
	Server        *server.Server
	SignalHandler *signals.Handler
	MemberlistKV  *memberlist.KVInitService
	ring          *ring.Ring
	usageReport   *usagestats.Reporter
	RuntimeConfig *runtimeconfig.Manager
	Overrides     *validation.Overrides
	Compactor     *compactor.MultitenantCompactor

	TenantLimits validation.TenantLimits

	storageBucket phlareobj.Bucket

	grpcGatewayMux *grpcgw.ServeMux

	auth connect.Option
}

func New(cfg Config) (*Phlare, error) {
	logger := initLogger(cfg.Server.LogFormat, cfg.Server.LogLevel)
	cfg.Server.Log = logger
	usagestats.Edition("oss")

	phlare := &Phlare{
		Cfg:    cfg,
		logger: logger,
		reg:    prometheus.DefaultRegisterer,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := phlare.setupModuleManager(); err != nil {
		return nil, err
	}

	runtime.SetMutexProfileFraction(cfg.SelfProfiling.MutexProfileFraction)
	runtime.SetBlockProfileRate(cfg.SelfProfiling.BlockProfileRate)

	if cfg.Tracing.Enabled {
		// Setting the environment variable JAEGER_AGENT_HOST enables tracing
		trace, err := wwtracing.NewFromEnv(fmt.Sprintf("pyroscope-%s", cfg.Target))
		if err != nil {
			level.Error(logger).Log("msg", "error in initializing tracing. tracing will not be enabled", "err", err)
		}
		if cfg.Tracing.ProfilingEnabled {
			opentracing.SetGlobalTracer(spanprofiler.NewTracer(opentracing.GlobalTracer()))
		}
		phlare.tracer = trace
	}

	phlare.auth = connect.WithInterceptors(tenant.NewAuthInterceptor(cfg.MultitenancyEnabled))
	phlare.Cfg.API.HTTPAuthMiddleware = util.AuthenticateUser(cfg.MultitenancyEnabled)
	phlare.Cfg.API.GrpcAuthMiddleware = phlare.auth

	return phlare, nil
}

func (f *Phlare) setupModuleManager() error {
	mm := modules.NewManager(f.logger)

	mm.RegisterModule(Storage, f.initStorage, modules.UserInvisibleModule)
	mm.RegisterModule(GRPCGateway, f.initGRPCGateway, modules.UserInvisibleModule)
	mm.RegisterModule(MemberlistKV, f.initMemberlistKV, modules.UserInvisibleModule)
	mm.RegisterModule(Ring, f.initRing, modules.UserInvisibleModule)
	mm.RegisterModule(RuntimeConfig, f.initRuntimeConfig, modules.UserInvisibleModule)
	mm.RegisterModule(Overrides, f.initOverrides, modules.UserInvisibleModule)
	mm.RegisterModule(OverridesExporter, f.initOverridesExporter)
	mm.RegisterModule(Ingester, f.initIngester)
	mm.RegisterModule(Server, f.initServer, modules.UserInvisibleModule)
	mm.RegisterModule(API, f.initAPI, modules.UserInvisibleModule)
	mm.RegisterModule(Distributor, f.initDistributor)
	mm.RegisterModule(Querier, f.initQuerier)
	mm.RegisterModule(StoreGateway, f.initStoreGateway)
	mm.RegisterModule(UsageReport, f.initUsageReport)
	mm.RegisterModule(QueryFrontend, f.initQueryFrontend)
	mm.RegisterModule(QueryScheduler, f.initQueryScheduler)
	mm.RegisterModule(Compactor, f.initCompactor)
	mm.RegisterModule(All, nil)

	// Add dependencies
	deps := map[string][]string{
		All: {Ingester, Distributor, QueryScheduler, QueryFrontend, Querier, StoreGateway},

		Server:            {GRPCGateway},
		API:               {Server},
		Distributor:       {Overrides, Ring, API, UsageReport},
		Querier:           {Overrides, API, MemberlistKV, Ring, UsageReport},
		QueryFrontend:     {OverridesExporter, API, MemberlistKV, UsageReport},
		QueryScheduler:    {Overrides, API, MemberlistKV, UsageReport},
		Ingester:          {Overrides, API, MemberlistKV, Storage, UsageReport},
		StoreGateway:      {API, Storage, Overrides, MemberlistKV, UsageReport},
		Compactor:         {API, Storage, Overrides, MemberlistKV, UsageReport},
		UsageReport:       {Storage, MemberlistKV},
		Overrides:         {RuntimeConfig},
		OverridesExporter: {Overrides, MemberlistKV},
		RuntimeConfig:     {API},
		Ring:              {API, MemberlistKV},
		MemberlistKV:      {API},
	}

	for mod, targets := range deps {
		if err := mm.AddDependency(mod, targets...); err != nil {
			return err
		}
	}

	f.deps = deps
	f.ModuleManager = mm

	return nil
}

// made here https://patorjk.com/software/taag/#p=display&f=Doom&t=grafana%20pyroscope
// also needed to replace all ` with '
var banner = `
                 / _|
  __ _ _ __ __ _| |_ __ _ _ __   __ _   _ __  _   _ _ __ ___  ___  ___ ___  _ __   ___
 / _' | '__/ _' |  _/ _' | '_ \ / _' | | '_ \| | | | '__/ _ \/ __|/ __/ _ \| '_ \ / _ \
| (_| | | | (_| | || (_| | | | | (_| | | |_) | |_| | | | (_) \__ \ (_| (_) | |_) |  __/
 \__, |_|  \__,_|_| \__,_|_| |_|\__,_| | .__/ \__, |_|  \___/|___/\___\___/| .__/ \___|
  __/ |                                | |     __/ |                       | |
 |___/                                 |_|    |___/                        |_|
 `

func (f *Phlare) Run() error {
	_ = cli.GradientBanner(banner, os.Stderr)

	serviceMap, err := f.ModuleManager.InitModuleServices(f.Cfg.Target...)
	if err != nil {
		return err
	}

	f.serviceMap = serviceMap
	var servs []services.Service
	for _, s := range serviceMap {
		servs = append(servs, s)
	}

	sm, err := services.NewManager(servs...)
	if err != nil {
		return err
	}
	f.Server.HTTP.Path("/ready").Methods("GET").Handler(f.readyHandler(sm))

	RegisterHealthServer(f.Server.HTTP, grpcutil.WithManager(sm))
	healthy := func() {
		level.Info(f.logger).Log("msg", "Pyroscope started", "version", version.Info())
		if os.Getenv("PYROSCOPE_PRINT_ROUTES") != "" {
			printRoutes(f.Server.HTTP)
		}

		// Start profiling when Pyroscope is ready
		if !f.Cfg.SelfProfiling.DisablePush && f.Cfg.Target.String() == All {
			_, err := pyroscope.Start(pyroscope.Config{
				ApplicationName: "pyroscope",
				ServerAddress:   fmt.Sprintf("http://%s:%d", "localhost", f.Cfg.Server.HTTPListenPort),
				Tags: map[string]string{
					"hostname": os.Getenv("HOSTNAME"),
					"target":   "all",
					"version":  version.Version,
				},
				ProfileTypes: []pyroscope.ProfileType{
					pyroscope.ProfileCPU,
					pyroscope.ProfileAllocObjects,
					pyroscope.ProfileAllocSpace,
					pyroscope.ProfileInuseObjects,
					pyroscope.ProfileInuseSpace,
					pyroscope.ProfileGoroutines,
					pyroscope.ProfileMutexCount,
					pyroscope.ProfileMutexDuration,
					pyroscope.ProfileBlockCount,
					pyroscope.ProfileBlockDuration,
				},
			})
			if err != nil {
				level.Warn(f.logger).Log("msg", "failed to start pyroscope", "err", err)
			}
		}
	}

	if err = f.API.RegisterCatchAll(); err != nil {
		return err
	}

	serviceFailed := func(service services.Service) {
		// if any service fails, stop entire Phlare
		sm.StopAsync()

		// let's find out which module failed
		for m, s := range serviceMap {
			if s == service {
				if service.FailureCase() == modules.ErrStopProcess {
					level.Info(f.logger).Log("msg", "received stop signal via return error", "module", m, "error", service.FailureCase())
				} else {
					level.Error(f.logger).Log("msg", "module failed", "module", m, "error", service.FailureCase())
				}
				return
			}
		}

		level.Error(f.logger).Log("msg", "module failed", "module", "unknown", "error", service.FailureCase())
	}

	sm.AddListener(services.NewManagerListener(healthy, f.stopped, serviceFailed))

	// Setup signal handler. If signal arrives, we stop the manager, which stops all the services.
	f.SignalHandler = signals.NewHandler(f.Server.Log)
	go func() {
		f.SignalHandler.Loop()
		sm.StopAsync()
	}()

	// Start all services. This can really only fail if some service is already
	// in other state than New, which should not be the case.
	err = sm.StartAsync(context.Background())
	if err == nil {
		// Wait until service manager stops. It can stop in two ways:
		// 1) Signal is received and manager is stopped.
		// 2) Any service fails.
		err = sm.AwaitStopped(context.Background())
	}

	// If there is no error yet (= service manager started and then stopped without problems),
	// but any service failed, report that failure as an error to caller.
	if err == nil {
		if failed := sm.ServicesByState()[services.Failed]; len(failed) > 0 {
			for _, f := range failed {
				if f.FailureCase() != modules.ErrStopProcess {
					// Details were reported via failure listener before
					err = errors.New("failed services")
					break
				}
			}
		}
	}

	return err
}

func (f *Phlare) readyHandler(sm *services.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sm.IsHealthy() {
			msg := bytes.Buffer{}
			msg.WriteString("Some services are not Running:\n")

			byState := map[services.State][]string{}
			for name, svc := range f.serviceMap {
				state := svc.State()
				byState[state] = append(byState[state], name)
			}

			states := lo.Keys(byState)
			sort.Slice(states, func(i, j int) bool { return states[i] < states[j] })

			for _, st := range states {
				sort.Strings(byState[st])
				msg.WriteString(fmt.Sprintf("%v: %v\n", st, byState[st]))
			}

			http.Error(w, msg.String(), http.StatusServiceUnavailable)
			return
		}

		util.WriteTextResponse(w, "ready")
	}
}

func (f *Phlare) stopped() {
	level.Info(f.logger).Log("msg", "Pyroscope stopped")
	if f.tracer != nil {
		if err := f.tracer.Close(); err != nil {
			level.Error(f.logger).Log("msg", "error closing tracing", "err", err)
		}
	}
}

func initLogger(logFormat string, logLevel dslog.Level) log.Logger {
	writer := log.NewSyncWriter(os.Stderr)
	logger := dslog.NewGoKitWithWriter(logFormat, writer)

	// use UTC timestamps and skip 5 stack frames.
	logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.Caller(5))

	// Must put the level filter last for efficiency.
	logger = level.NewFilter(logger, logLevel.Option)

	return logger
}

func (f *Phlare) initAPI() (services.Service, error) {
	a, err := api.New(f.Cfg.API, f.Server, f.grpcGatewayMux, f.Server.Log)
	if err != nil {
		return nil, err
	}
	f.API = a

	if err := f.API.RegisterAPI(f.statusService()); err != nil {
		return nil, err
	}

	return nil, nil
}

func printRoutes(r *mux.Router) {
	err := r.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		path, err := route.GetPathRegexp()
		if err != nil {
			fmt.Printf("failed to get path regexp %s\n", err)
			return nil
		}
		method, err := route.GetMethods()
		if err != nil {
			method = []string{"*"}
		}
		fmt.Printf("%s %s\n", strings.Join(method, ","), path)
		return nil
	})
	if err != nil {
		fmt.Printf("failed to walk routes %s\n", err)
	}
}
