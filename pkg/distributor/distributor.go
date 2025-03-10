package distributor

import (
	"bytes"
	"context"
	"expvar"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/dustin/go-humanize"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/uuid"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/limiter"
	"github.com/grafana/dskit/ring"
	ring_client "github.com/grafana/dskit/ring/client"
	"github.com/grafana/dskit/services"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"go.uber.org/atomic"

	googlev1 "github.com/grafana/pyroscope/api/gen/proto/go/google/v1"
	pushv1 "github.com/grafana/pyroscope/api/gen/proto/go/push/v1"
	typesv1 "github.com/grafana/pyroscope/api/gen/proto/go/types/v1"
	"github.com/grafana/pyroscope/pkg/clientpool"
	distributormodel "github.com/grafana/pyroscope/pkg/distributor/model"
	phlaremodel "github.com/grafana/pyroscope/pkg/model"
	"github.com/grafana/pyroscope/pkg/pprof"
	"github.com/grafana/pyroscope/pkg/slices"
	"github.com/grafana/pyroscope/pkg/tenant"
	"github.com/grafana/pyroscope/pkg/usagestats"
	"github.com/grafana/pyroscope/pkg/util"
	"github.com/grafana/pyroscope/pkg/validation"
)

type PushClient interface {
	Push(context.Context, *connect.Request[pushv1.PushRequest]) (*connect.Response[pushv1.PushResponse], error)
}

const (
	// distributorRingKey is the key under which we store the distributors ring in the KVStore.
	distributorRingKey = "distributor"

	// ringAutoForgetUnhealthyPeriods is how many consecutive timeout periods an unhealthy instance
	// in the ring will be automatically removed after.
	ringAutoForgetUnhealthyPeriods = 10

	ProfileName = "__name__"
)

// Config for a Distributor.
type Config struct {
	PushTimeout time.Duration
	PoolConfig  clientpool.PoolConfig `yaml:"pool_config,omitempty"`

	// Distributors ring
	DistributorRing util.CommonRingConfig `yaml:"ring" doc:"hidden"`
}

// RegisterFlags registers distributor-related flags.
func (cfg *Config) RegisterFlags(fs *flag.FlagSet, logger log.Logger) {
	cfg.PoolConfig.RegisterFlagsWithPrefix("distributor", fs)
	fs.DurationVar(&cfg.PushTimeout, "distributor.push.timeout", 5*time.Second, "Timeout when pushing data to ingester.")
	cfg.DistributorRing.RegisterFlags("distributor.ring.", "collectors/", "distributors", fs, logger)
}

// Distributor coordinates replicates and distribution of log streams.
type Distributor struct {
	services.Service
	logger log.Logger

	cfg           Config
	limits        Limits
	ingestersRing ring.ReadRing
	pool          *ring_client.Pool

	// The global rate limiter requires a distributors ring to count
	// the number of healthy instances
	distributorsLifecycler *ring.BasicLifecycler
	distributorsRing       *ring.Ring
	healthyInstancesCount  *atomic.Uint32
	ingestionRateLimiter   *limiter.RateLimiter

	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher

	// Metrics and stats.
	metrics                 *metrics
	rfStats                 *expvar.Int
	bytesReceivedStats      *usagestats.Statistics
	bytesReceivedTotalStats *usagestats.Counter
	profileReceivedStats    *usagestats.Counter
}

type Limits interface {
	IngestionRateBytes(tenantID string) float64
	IngestionBurstSizeBytes(tenantID string) int
	IngestionTenantShardSize(tenantID string) int
	MaxLabelNameLength(tenantID string) int
	MaxLabelValueLength(tenantID string) int
	MaxLabelNamesPerSeries(tenantID string) int
	MaxProfileSizeBytes(tenantID string) int
	MaxProfileStacktraceSamples(tenantID string) int
	MaxProfileStacktraceSampleLabels(tenantID string) int
	MaxProfileStacktraceDepth(tenantID string) int
	MaxProfileSymbolValueLength(tenantID string) int
	MaxSessionsPerSeries(tenantID string) int
	validation.ProfileValidationLimits
}

func New(cfg Config, ingestersRing ring.ReadRing, factory ring_client.PoolFactory, limits Limits, reg prometheus.Registerer, logger log.Logger, clientsOptions ...connect.ClientOption) (*Distributor, error) {
	clients := promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Namespace: "pyroscope",
		Name:      "distributor_ingester_clients",
		Help:      "The current number of ingester clients.",
	})
	d := &Distributor{
		cfg:                     cfg,
		logger:                  logger,
		ingestersRing:           ingestersRing,
		pool:                    clientpool.NewIngesterPool(cfg.PoolConfig, ingestersRing, factory, clients, logger, clientsOptions...),
		metrics:                 newMetrics(reg),
		healthyInstancesCount:   atomic.NewUint32(0),
		limits:                  limits,
		rfStats:                 usagestats.NewInt("distributor_replication_factor"),
		bytesReceivedStats:      usagestats.NewStatistics("distributor_bytes_received"),
		bytesReceivedTotalStats: usagestats.NewCounter("distributor_bytes_received_total"),
		profileReceivedStats:    usagestats.NewCounter("distributor_profiles_received"),
	}
	var err error

	subservices := []services.Service(nil)
	subservices = append(subservices, d.pool)

	distributorsRing, distributorsLifecycler, err := newRingAndLifecycler(cfg.DistributorRing, d.healthyInstancesCount, logger, reg)
	if err != nil {
		return nil, err
	}

	subservices = append(subservices, distributorsLifecycler, distributorsRing)

	d.ingestionRateLimiter = limiter.NewRateLimiter(newGlobalRateStrategy(newIngestionRateStrategy(limits), d), 10*time.Second)
	d.distributorsLifecycler = distributorsLifecycler
	d.distributorsRing = distributorsRing

	d.subservices, err = services.NewManager(subservices...)
	if err != nil {
		return nil, errors.Wrap(err, "services manager")
	}
	d.subservicesWatcher = services.NewFailureWatcher()
	d.subservicesWatcher.WatchManager(d.subservices)

	d.Service = services.NewBasicService(d.starting, d.running, d.stopping)
	d.rfStats.Set(int64(ingestersRing.ReplicationFactor()))
	d.metrics.replicationFactor.Set(float64(ingestersRing.ReplicationFactor()))
	return d, nil
}

func (d *Distributor) starting(ctx context.Context) error {
	return services.StartManagerAndAwaitHealthy(ctx, d.subservices)
}

func (d *Distributor) running(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-d.subservicesWatcher.Chan():
		return errors.Wrap(err, "distributor subservice failed")
	}
}

func (d *Distributor) stopping(_ error) error {
	return services.StopManagerAndAwaitStopped(context.Background(), d.subservices)
}

func (d *Distributor) Push(ctx context.Context, grpcReq *connect.Request[pushv1.PushRequest]) (*connect.Response[pushv1.PushResponse], error) {
	req := &distributormodel.PushRequest{
		Series: make([]*distributormodel.ProfileSeries, 0, len(grpcReq.Msg.Series)),
	}

	// All allocated pprof.Profile instances must be closed after use.
	var n int
	for i := 0; i < len(grpcReq.Msg.Series); i++ {
		n += len(grpcReq.Msg.Series[i].Samples)
	}
	profiles := make([]*pprof.Profile, 0, n)
	defer func() {
		for _, p := range profiles {
			if p.Profile != nil {
				p.Close()
			}
		}
	}()

	for _, grpcSeries := range grpcReq.Msg.Series {
		series := &distributormodel.ProfileSeries{
			Labels:  grpcSeries.Labels,
			Samples: make([]*distributormodel.ProfileSample, 0, len(grpcSeries.Samples)),
		}
		for _, grpcSample := range grpcSeries.Samples {
			profile, err := pprof.RawFromBytes(grpcSample.RawProfile)
			if err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
			profiles = append(profiles, profile)
			sample := &distributormodel.ProfileSample{
				Profile:    profile,
				RawProfile: grpcSample.RawProfile,
				ID:         grpcSample.ID,
			}
			req.RawProfileSize += len(grpcSample.RawProfile)
			series.Samples = append(series.Samples, sample)
		}
		req.Series = append(req.Series, series)
	}

	return d.PushParsed(ctx, req)
}

func (d *Distributor) PushParsed(ctx context.Context, req *distributormodel.PushRequest) (*connect.Response[pushv1.PushResponse], error) {
	now := model.Now()
	tenantID, err := tenant.ExtractTenantIDFromContext(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	var (
		totalPushUncompressedBytes int64
		totalProfiles              int64
	)

	for _, series := range req.Series {
		serviceName := phlaremodel.Labels(series.Labels).Get(phlaremodel.LabelNameServiceName)
		if serviceName == "" {
			series.Labels = append(series.Labels, &typesv1.LabelPair{Name: phlaremodel.LabelNameServiceName, Value: "unspecified"})
		}
		sort.Sort(phlaremodel.Labels(series.Labels))
	}

	haveRawPprof := req.RawProfileType == distributormodel.RawProfileTypePPROF
	d.bytesReceivedTotalStats.Inc(int64(req.RawProfileSize))
	d.bytesReceivedStats.Record(float64(req.RawProfileSize))
	if !haveRawPprof {
		// if a single profile contains multiple profile types/names (e.g. jfr) then there is no such thing as
		// compressed size per profile type as all profile types are compressed once together. So we can not count
		// compressed bytes per profile type. Instead we count compressed bytes per profile.
		profName := req.RawProfileType // use "jfr" as profile name
		d.metrics.receivedCompressedBytes.WithLabelValues(string(profName), tenantID).Observe(float64(req.RawProfileSize))
	}

	for _, series := range req.Series {
		// include the labels in the size calculation
		for _, lbs := range series.Labels {
			totalPushUncompressedBytes += int64(len(lbs.Name))
			totalPushUncompressedBytes += int64(len(lbs.Value))
		}
		profName := phlaremodel.Labels(series.Labels).Get(ProfileName)
		series.Labels = d.limitMaxSessionsPerSeries(tenantID, series.Labels)
		for _, raw := range series.Samples {
			usagestats.NewCounter(fmt.Sprintf("distributor_profile_type_%s_received", profName)).Inc(1)
			d.profileReceivedStats.Inc(1)
			if haveRawPprof {
				d.metrics.receivedCompressedBytes.WithLabelValues(profName, tenantID).Observe(float64(len(raw.RawProfile)))
			}
			totalProfiles++
			p := raw.Profile
			var decompressedSize int
			if haveRawPprof {
				decompressedSize = p.SizeBytes()
			} else {
				decompressedSize = p.SizeVT()
			}
			d.metrics.receivedDecompressedBytes.WithLabelValues(profName, tenantID).Observe(float64(decompressedSize))
			d.metrics.receivedSamples.WithLabelValues(profName, tenantID).Observe(float64(len(p.Sample)))
			totalPushUncompressedBytes += int64(decompressedSize)

			if err = validation.ValidateProfile(d.limits, tenantID, p.Profile, decompressedSize, series.Labels, now); err != nil {
				// todo this actually discards more if multiple Samples in a Series request
				_ = level.Debug(d.logger).Log("msg", "invalid profile", "err", err)
				validation.DiscardedProfiles.WithLabelValues(string(validation.ReasonOf(err)), tenantID).Add(float64(totalProfiles))
				validation.DiscardedBytes.WithLabelValues(string(validation.ReasonOf(err)), tenantID).Add(float64(totalPushUncompressedBytes))
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}

			symbolsSize, samplesSize := profileSizeBytes(p.Profile)
			d.metrics.receivedSamplesBytes.WithLabelValues(profName, tenantID).Observe(float64(samplesSize))
			d.metrics.receivedSymbolsBytes.WithLabelValues(profName, tenantID).Observe(float64(symbolsSize))
		}
	}

	if totalProfiles == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("no profiles received"))
	}

	// rate limit the request
	if !d.ingestionRateLimiter.AllowN(time.Now(), tenantID, int(totalPushUncompressedBytes)) {
		validation.DiscardedProfiles.WithLabelValues(string(validation.RateLimited), tenantID).Add(float64(totalProfiles))
		validation.DiscardedBytes.WithLabelValues(string(validation.RateLimited), tenantID).Add(float64(totalPushUncompressedBytes))
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("push rate limit (%s) exceeded while adding %s", humanize.IBytes(uint64(d.limits.IngestionRateBytes(tenantID))), humanize.IBytes(uint64(totalPushUncompressedBytes))),
		)
	}

	// Next we split profiles by labels. Newly allocated profiles should be closed after use.
	profileSeries, newProfiles := extractSampleSeries(req)
	defer func() {
		for _, p := range newProfiles {
			p.Close()
		}
	}()

	// Validate the labels again and generate tokens for shuffle sharding.
	keys := make([]uint32, len(profileSeries))
	for i, series := range profileSeries {
		if err = validation.ValidateLabels(d.limits, tenantID, series.Labels); err != nil {
			validation.DiscardedProfiles.WithLabelValues(string(validation.ReasonOf(err)), tenantID).Add(float64(totalProfiles))
			validation.DiscardedBytes.WithLabelValues(string(validation.ReasonOf(err)), tenantID).Add(float64(totalPushUncompressedBytes))
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		keys[i] = TokenFor(tenantID, phlaremodel.LabelPairsString(series.Labels))
	}

	profiles := make([]*profileTracker, 0, len(profileSeries))
	for _, series := range profileSeries {
		for _, raw := range series.Samples {
			p := raw.Profile
			// zip the data back into the buffer
			bw := bytes.NewBuffer(raw.RawProfile[:0])
			if _, err := p.WriteTo(bw); err != nil {
				return nil, err
			}
			raw.ID = uuid.NewString()
			raw.RawProfile = bw.Bytes()
		}
		profiles = append(profiles, &profileTracker{profile: series})
	}

	const maxExpectedReplicationSet = 5 // typical replication factor 3 plus one for inactive plus one for luck
	var descs [maxExpectedReplicationSet]ring.InstanceDesc

	samplesByIngester := map[string][]*profileTracker{}
	ingesterDescs := map[string]ring.InstanceDesc{}
	for i, key := range keys {
		// Get a subring if tenant has shuffle shard size configured.
		subRing := d.ingestersRing.ShuffleShard(tenantID, d.limits.IngestionTenantShardSize(tenantID))

		replicationSet, err := subRing.Get(key, ring.Write, descs[:0], nil, nil)
		if err != nil {
			return nil, err
		}
		profiles[i].minSuccess = len(replicationSet.Instances) - replicationSet.MaxErrors
		profiles[i].maxFailures = replicationSet.MaxErrors
		for _, ingester := range replicationSet.Instances {
			samplesByIngester[ingester.Addr] = append(samplesByIngester[ingester.Addr], profiles[i])
			ingesterDescs[ingester.Addr] = ingester
		}
	}
	tracker := pushTracker{
		done: make(chan struct{}, 1), // buffer avoids blocking if caller terminates - sendProfiles() only sends once on each
		err:  make(chan error, 1),
	}
	tracker.samplesPending.Store(int32(len(profiles)))
	for ingester, samples := range samplesByIngester {
		go func(ingester ring.InstanceDesc, samples []*profileTracker) {
			// Use a background context to make sure all ingesters get samples even if we return early
			localCtx, cancel := context.WithTimeout(context.Background(), d.cfg.PushTimeout)
			defer cancel()
			localCtx = tenant.InjectTenantID(localCtx, tenantID)
			if sp := opentracing.SpanFromContext(ctx); sp != nil {
				localCtx = opentracing.ContextWithSpan(localCtx, sp)
			}
			d.sendProfiles(localCtx, ingester, samples, &tracker)
		}(ingesterDescs[ingester], samples)
	}
	select {
	case err := <-tracker.err:
		return nil, err
	case <-tracker.done:
		return connect.NewResponse(&pushv1.PushResponse{}), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// profileSizeBytes returns the size of symbols and samples in bytes.
func profileSizeBytes(p *googlev1.Profile) (symbols, samples int64) {
	fullSize := p.SizeVT()
	// remove samples
	samplesSlice := p.Sample
	p.Sample = nil

	symbols = int64(p.SizeVT())
	samples = int64(fullSize) - symbols

	// count labels in samples
	samplesLabels := 0
	for _, s := range samplesSlice {
		for _, l := range s.Label {
			samplesLabels += len(p.StringTable[l.Key]) + len(p.StringTable[l.Str]) + len(p.StringTable[l.NumUnit])
		}
	}
	symbols -= int64(samplesLabels)
	samples += int64(samplesLabels)

	// restore samples
	p.Sample = samplesSlice
	return
}

func (d *Distributor) sendProfiles(ctx context.Context, ingester ring.InstanceDesc, profileTrackers []*profileTracker, pushTracker *pushTracker) {
	err := d.sendProfilesErr(ctx, ingester, profileTrackers)
	// If we succeed, decrement each sample's pending count by one.  If we reach
	// the required number of successful puts on this sample, then decrement the
	// number of pending samples by one.  If we successfully push all samples to
	// min success ingesters, wake up the waiting rpc so it can return early.
	// Similarly, track the number of errors, and if it exceeds maxFailures
	// shortcut the waiting rpc.
	//
	// The use of atomic increments here guarantees only a single sendSamples
	// goroutine will write to either channel.
	for i := range profileTrackers {
		if err != nil {
			if profileTrackers[i].failed.Inc() <= int32(profileTrackers[i].maxFailures) {
				continue
			}
			if pushTracker.samplesFailed.Inc() == 1 {
				pushTracker.err <- err
			}
		} else {
			if profileTrackers[i].succeeded.Inc() != int32(profileTrackers[i].minSuccess) {
				continue
			}
			if pushTracker.samplesPending.Dec() == 0 {
				pushTracker.done <- struct{}{}
			}
		}
	}
}

func (d *Distributor) sendProfilesErr(ctx context.Context, ingester ring.InstanceDesc, profileTrackers []*profileTracker) error {
	c, err := d.pool.GetClientFor(ingester.Addr)
	if err != nil {
		return err
	}

	req := connect.NewRequest(&pushv1.PushRequest{
		Series: make([]*pushv1.RawProfileSeries, 0, len(profileTrackers)),
	})

	for _, p := range profileTrackers {
		series := &pushv1.RawProfileSeries{
			Labels:  p.profile.Labels,
			Samples: make([]*pushv1.RawSample, 0, len(p.profile.Samples)),
		}
		for _, sample := range p.profile.Samples {
			series.Samples = append(series.Samples, &pushv1.RawSample{
				RawProfile: sample.RawProfile,
				ID:         sample.ID,
			})
		}
		req.Msg.Series = append(req.Msg.Series, series)
	}

	_, err = c.(PushClient).Push(ctx, req)
	return err
}

func (d *Distributor) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if d.distributorsRing != nil {
		d.distributorsRing.ServeHTTP(w, req)
	} else {
		ringNotEnabledPage := `
			<!DOCTYPE html>
			<html>
				<head>
					<meta charset="UTF-8">
					<title>Distributor Status</title>
				</head>
				<body>
					<h1>Distributor Status</h1>
					<p>Distributor is not running with global limits enabled</p>
				</body>
			</html>`
		util.WriteHTMLResponse(w, ringNotEnabledPage)
	}
}

// HealthyInstancesCount implements the ReadLifecycler interface
//
// We use a ring lifecycler delegate to count the number of members of the
// ring. The count is then used to enforce rate limiting correctly for each
// distributor. $EFFECTIVE_RATE_LIMIT = $GLOBAL_RATE_LIMIT / $NUM_INSTANCES
func (d *Distributor) HealthyInstancesCount() int {
	return int(d.healthyInstancesCount.Load())
}

func extractSampleSeries(req *distributormodel.PushRequest) ([]*distributormodel.ProfileSeries, []*pprof.Profile) {
	profileSeries := make([]*distributormodel.ProfileSeries, 0, len(req.Series))
	newProfiles := make([]*pprof.Profile, 0, 2*len(req.Series))
	for _, series := range req.Series {
		s := &distributormodel.ProfileSeries{
			Labels:  series.Labels,
			Samples: make([]*distributormodel.ProfileSample, 0, len(series.Samples)),
		}
		for _, raw := range series.Samples {
			raw.Profile.Normalize()
			pprof.RenameLabel(raw.Profile.Profile, pprof.ProfileIDLabelName, pprof.SpanIDLabelName)
			groups := pprof.GroupSamplesWithoutLabels(raw.Profile.Profile, pprof.SpanIDLabelName)
			if len(groups) == 0 || (len(groups) == 1 && len(groups[0].Labels) == 0) {
				// No sample labels in the profile.
				// We do not modify the request.
				s.Samples = append(s.Samples, raw)
				continue
			}
			e := pprof.NewSampleExporter(raw.Profile.Profile)
			for _, group := range groups {
				// exportSamples creates a new profile with the samples provided.
				// The samples are obtained via GroupSamples call, which means
				// the underlying capacity is referenced by the source profile.
				// Therefore, the slice has to be copied and samples zeroed to
				// avoid ownership issues.
				profile := exportSamples(e, group.Samples)
				newProfiles = append(newProfiles, profile)
				// Note that group.Labels reference strings from the source profile.
				labels := mergeSeriesAndSampleLabels(raw.Profile.Profile, series.Labels, group.Labels)
				profileSeries = append(profileSeries, &distributormodel.ProfileSeries{
					Labels:  labels,
					Samples: []*distributormodel.ProfileSample{{Profile: profile}},
				})
			}
		}
		if len(s.Samples) > 0 {
			profileSeries = append(profileSeries, s)
		}
	}
	return profileSeries, newProfiles
}

func (d *Distributor) limitMaxSessionsPerSeries(tenantID string, labels phlaremodel.Labels) phlaremodel.Labels {
	maxSessionsPerSeries := d.limits.MaxSessionsPerSeries(tenantID)
	if maxSessionsPerSeries == 0 {
		return labels.Delete(phlaremodel.LabelNameSessionID)
	}
	sessionIDLabel, ok := labels.GetLabel(phlaremodel.LabelNameSessionID)
	if !ok {
		return labels
	}
	sessionID, err := phlaremodel.ParseSessionID(sessionIDLabel.Value)
	if err != nil {
		_ = level.Debug(d.logger).Log("msg", "invalid session_id", "err", err)
		return labels.Delete(phlaremodel.LabelNameSessionID)
	}
	sessionIDLabel.Value = phlaremodel.SessionID(int(sessionID) % maxSessionsPerSeries).String()
	return labels
}

// mergeSeriesAndSampleLabels merges sample labels with
// series labels. Series labels take precedence.
func mergeSeriesAndSampleLabels(p *googlev1.Profile, sl []*typesv1.LabelPair, pl []*googlev1.Label) []*typesv1.LabelPair {
	m := phlaremodel.Labels(sl).Clone()
	for _, l := range pl {
		m = append(m, &typesv1.LabelPair{
			Name:  p.StringTable[l.Key],
			Value: p.StringTable[l.Str],
		})
	}
	sort.Stable(m)
	return m.Unique()
}

func exportSamples(e *pprof.SampleExporter, samples []*googlev1.Sample) *pprof.Profile {
	samplesCopy := make([]*googlev1.Sample, len(samples))
	copy(samplesCopy, samples)
	slices.Clear(samples)
	n := pprof.NewProfile()
	e.ExportSamples(n.Profile, samplesCopy)
	return n
}

type profileTracker struct {
	profile     *distributormodel.ProfileSeries
	minSuccess  int
	maxFailures int
	succeeded   atomic.Int32
	failed      atomic.Int32
}

type pushTracker struct {
	samplesPending atomic.Int32
	samplesFailed  atomic.Int32
	done           chan struct{}
	err            chan error
}

// TokenFor generates a token used for finding ingesters from ring
func TokenFor(tenantID, labels string) uint32 {
	h := fnv.New32()
	_, _ = h.Write([]byte(tenantID))
	_, _ = h.Write([]byte(labels))
	return h.Sum32()
}

// newRingAndLifecycler creates a new distributor ring and lifecycler with all required lifecycler delegates
func newRingAndLifecycler(cfg util.CommonRingConfig, instanceCount *atomic.Uint32, logger log.Logger, reg prometheus.Registerer) (*ring.Ring, *ring.BasicLifecycler, error) {
	reg = prometheus.WrapRegistererWithPrefix("pyroscope_", reg)
	kvStore, err := kv.NewClient(cfg.KVStore, ring.GetCodec(), kv.RegistererWithKVName(reg, "distributor-lifecycler"), logger)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize distributors' KV store")
	}

	lifecyclerCfg, err := toBasicLifecyclerConfig(cfg, logger)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to build distributors' lifecycler config")
	}

	var delegate ring.BasicLifecyclerDelegate
	delegate = ring.NewInstanceRegisterDelegate(ring.ACTIVE, lifecyclerCfg.NumTokens)
	delegate = newHealthyInstanceDelegate(instanceCount, cfg.HeartbeatTimeout, delegate)
	delegate = ring.NewLeaveOnStoppingDelegate(delegate, logger)
	delegate = ring.NewAutoForgetDelegate(ringAutoForgetUnhealthyPeriods*cfg.HeartbeatTimeout, delegate, logger)

	distributorsLifecycler, err := ring.NewBasicLifecycler(lifecyclerCfg, "distributor", distributorRingKey, kvStore, delegate, logger, reg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize distributors' lifecycler")
	}

	distributorsRing, err := ring.New(cfg.ToRingConfig(), "distributor", distributorRingKey, logger, reg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize distributors' ring client")
	}

	return distributorsRing, distributorsLifecycler, nil
}
