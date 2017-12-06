package plugins

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/hexdecteam/easegateway-types/pipelines"
	"github.com/hexdecteam/easegateway-types/plugins"
	"github.com/hexdecteam/easegateway-types/task"

	"common"
	"logger"
)

type latencyLimiterConfig struct {
	common.PluginCommonConfig
	AllowMSec                uint16   `json:"allow_msec"`           // up to 65535
	BackOffTimeoutMSec       int16    `json:"backoff_timeout_msec"` // zero means no queuing, -1 means no timeout
	FlowControlPercentageKey string   `json:"flow_control_percentage_key"`
	LatencyThresholdMSec     uint32   `json:"latency_threshold_msec"` // up to 4294967295
	PluginsConcerned         []string `json:"plugins_concerned"`
	ProbePercentage          uint8    `json:"probe_percentage"` // [1~99]
}

func latencyLimiterConfigConstructor() plugins.Config {
	return &latencyLimiterConfig{
		LatencyThresholdMSec: 800,
		BackOffTimeoutMSec:   1000,
		AllowMSec:            1000,
		ProbePercentage:      10,
	}
}

func (c *latencyLimiterConfig) Prepare(pipelineNames []string) error {
	err := c.PluginCommonConfig.Prepare(pipelineNames)
	if err != nil {
		return err
	}

	if len(c.PluginsConcerned) == 0 {
		return fmt.Errorf("invalid plugins concerned")
	}

	for _, pluginName := range c.PluginsConcerned {
		if len(strings.TrimSpace(pluginName)) == 0 {
			return fmt.Errorf("invalid plugin name")
		}
	}

	if c.LatencyThresholdMSec < 1 {
		return fmt.Errorf("invalid latency millisecond threshold")
	}

	if c.BackOffTimeoutMSec < -1 {
		return fmt.Errorf("invalid queuing timeout, must be >= -1")
	} else if c.BackOffTimeoutMSec == -1 {
		logger.Warnf("[INFINITE timeout of latency limit has been applied, " +
			"no request could be timed out from back off!]")
	} else if c.BackOffTimeoutMSec == 0 {
		logger.Warnf("[ZERO timeout of latency limit has been applied, " +
			"no request could be backed off by limiter!]")
	} else if c.BackOffTimeoutMSec > 10000 {
		return fmt.Errorf("invalid backoff timeout millisecond (requires less than or equal to 10 seconds)")
	}

	if c.ProbePercentage >= 100 || c.ProbePercentage < 1 {
		return fmt.Errorf("invalid probe percentage (requires bigger than zero and less than 100)")
	}
	c.FlowControlPercentageKey = strings.TrimSpace(c.FlowControlPercentageKey)

	return nil
}

////

type latencyWindowLimiter struct {
	conf *latencyLimiterConfig
}

func latencyLimiterConstructor(conf plugins.Config) (plugins.Plugin, plugins.PluginType, error) {
	c, ok := conf.(*latencyLimiterConfig)
	if !ok {
		return nil, plugins.ProcessPlugin, fmt.Errorf("config type want *latencyWindowLimiterConfig got %T", conf)
	}

	l := &latencyWindowLimiter{
		conf: c,
	}

	return l, plugins.ProcessPlugin, nil
}

func (l *latencyWindowLimiter) Prepare(ctx pipelines.PipelineContext) {
	// Register as plugin level indicator, so we don't need to unregister them in CleanUp()
	registerPluginIndicatorForLimiter(ctx, l.Name(), pipelines.STATISTICS_INDICATOR_FOR_ALL_PLUGIN_INSTANCE)
}

// Probe: don't totally fuse outbound requests because we need small amount of requests to probe the concerned target
func (l *latencyWindowLimiter) isProbe(outboundRate float64, inboundRate float64) bool {
	if outboundRate >= 10 && 100*outboundRate/inboundRate > float64(l.conf.ProbePercentage) || // outbound rate is big enough so don't need probe
		inboundRate >= 10 && // rand.Intn(1) will always return zero
			rand.Int31n(100) >= int32(l.conf.ProbePercentage) {
		return false
	}
	return true
}

func (l *latencyWindowLimiter) Run(ctx pipelines.PipelineContext, t task.Task) error {
	t.AddFinishedCallback(fmt.Sprintf("%s-checkLatency", l.Name()),
		getTaskFinishedCallbackInLatencyLimiter(ctx, l.conf.PluginsConcerned, l.conf.LatencyThresholdMSec, l.conf.AllowMSec, l.Name()))

	go updateInboundThroughputRate(ctx, l.Name()) // ignore error if it occurs

	counter, err := getLatencyLimiterCounter(ctx, l.Name(), l.conf.AllowMSec)
	if err != nil {
		return nil
	}

	r, err := getInboundThroughputRate1(ctx, l.Name())
	if err != nil {
		logger.Warnf("[BUG: query state data for pipeline %s failed, "+
			"ignored to limit request: %v]", ctx.PipelineName(), err)
		return nil
	}

	outboundRate, err := ctx.Statistics().PluginThroughputRate1(l.Name(), pipelines.AllStatistics)
	if err != nil {
		logger.Warnf("[BUG: query state data for pipeline %s failed, "+
			"ignored to limit request: %v]", ctx.PipelineName(), err)
	}

	inboundRate, _ := r.Get()                                                                    // ignore error safely
	// use l.conf.AllowMSec to avoid thrashing caused by network, upstream server gc or other factors
	counterThreshold := uint64(float64(l.conf.AllowMSec) / 1000.0 * outboundRate)
	count := counter.Count()
	logger.Debugf("[inboundRate: %.3f, outboundRate: %.3f, counter: %d, counterThreshold: %d]", inboundRate, outboundRate, counter.Count(), counterThreshold)
	if count > counterThreshold { // needs flow control
		go updateFlowControlledThroughputRate(ctx, l.Name())

		if !l.isProbe(outboundRate, inboundRate) {
			if l.conf.BackOffTimeoutMSec == 0 { // don't back off
				// service fusing
				t.SetError(fmt.Errorf("service is unavaialbe caused by latency limit"),
					task.ResultFlowControl)
				return nil
			}
			var backOffTimeout <-chan time.Time
			if l.conf.BackOffTimeoutMSec != -1 {
				backOffTimeout = time.After(time.Duration(l.conf.BackOffTimeoutMSec) * time.Millisecond)
			}

			backOffStep := 10
			if int(l.conf.BackOffTimeoutMSec) <= backOffStep {
				backOffStep = 1
			} else {
				backOffStep = int(l.conf.BackOffTimeoutMSec / 10)
			}
			// wait until timeout, cancel or latency recoveryed
		LOOP:
			for {
				select {
				case <-backOffTimeout: // receive on a nil channel will always block
					t.SetError(fmt.Errorf("service is unavailable caused by latency limit backoff timeout"),
						task.ResultFlowControl)
					return nil
				case <-time.After(time.Duration(backOffStep) * time.Millisecond):
					if counter.Count() < counterThreshold {
						logger.Debugf("[successfully passed latency limiter after backed off]")
						break LOOP
					}
				case <-t.Cancel():
					err := fmt.Errorf("task is cancelled by %s", t.CancelCause())
					t.SetError(err, task.ResultTaskCancelled)
					return t.Error()
				}
			}
		}
	}

	if len(l.conf.FlowControlPercentageKey) != 0 {
		percentage, err := getFlowControlledPercentage(ctx, l.Name())
		if err != nil {
			logger.Warnf("[BUG: query flow control percentage data for pipeline %s failed, "+
				"ignored this output]", ctx.PipelineName(), err)
		} else {
			t.WithValue(l.conf.FlowControlPercentageKey, percentage)
		}
	}

	return nil
}

func (l *latencyWindowLimiter) Name() string {
	return l.conf.PluginName()
}

func (h *latencyWindowLimiter) CleanUp(ctx pipelines.PipelineContext) {
	// Nothing to do.
}

func (l *latencyWindowLimiter) Close() {
	// Nothing to do.
}

////

const (
	latencyLimiterCounterKey = "latencyLimiterCounter"
)

// latencyLimiterCounter count the number of requests that reached the latency limiter
// threshold. It is increased when the request reached the lantency threshold and decreased when
// below.
//
// The maximum counter will be math.max(1, maxCountMSec/1000.0 * outBoundThroughputRate1)
type latencyLimiterCounter struct {
	c       chan *bool
	counter uint64
	closed  bool
}

func newLatencyLimiterCounter(ctx pipelines.PipelineContext, pluginName string, maxCountMSec uint16) *latencyLimiterCounter {
	ret := &latencyLimiterCounter{
		c: make(chan *bool, 32767),
	}

	go func() {
		for {
			select {
			case f := <-ret.c:
				if f == nil {
					return // channel/counter closed, exit
				} else if *f { // increase
					if outboundRate, err := ctx.Statistics().PluginThroughputRate1(pluginName, pipelines.AllStatistics); err == nil {
						max := uint64(outboundRate*float64(maxCountMSec)/1000.0 + 0.5)
						if max == 0 {
							max = 1
						}
						logger.Debugf("[increase counter: %d, counter max: %d, outboundRate: %.1f]", ret.counter, max, outboundRate)
						ret.counter += 1
						if ret.counter > max { // max is not a fixed number, so we may need to adjust ret.counter
							ret.counter = max
						}
					}
				} else if ret.counter > 0 { // decrease
					ret.counter = ret.counter / 2 // fast recovery
				}
			}
		}
	}()

	return ret
}

func (c *latencyLimiterCounter) Increase() {
	if !c.closed {
		f := true
		c.c <- &f
	}
}

func (c *latencyLimiterCounter) Decrease() {
	if !c.closed {
		f := false
		c.c <- &f
	}
}

func (c *latencyLimiterCounter) Count() uint64 {
	if c.closed {
		return 0
	}

	for len(c.c) > 0 { // wait counter is updated completely by spin
		time.Sleep(time.Millisecond)
	}

	return c.counter
}

func (c *latencyLimiterCounter) Close() error { // io.Closer stub
	c.closed = true
	close(c.c)
	return nil
}

func getLatencyLimiterCounter(ctx pipelines.PipelineContext, pluginName string, allowMSec uint16) (*latencyLimiterCounter, error) {
	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	counter, err := bucket.QueryDataWithBindDefault(latencyLimiterCounterKey,
		func() interface{} {
			return newLatencyLimiterCounter(ctx, pluginName, 2*allowMSec) // maxCountMSec may needs tuned
		})
	if err != nil {
		logger.Warnf("[BUG: query state data for pipeline %s failed, "+
			"ignored to limit request: %v]", ctx.PipelineName(), err)
		return nil, err
	}

	return counter.(*latencyLimiterCounter), nil
}

func getTaskFinishedCallbackInLatencyLimiter(ctx pipelines.PipelineContext, pluginsConcerned []string,
	latencyThresholdMSec uint32, allowMSec uint16, pluginName string) task.TaskFinished {

	return func(t1 task.Task, _ task.TaskStatus) {
		var latency float64
		var found bool
		latencyThreshold := float64(time.Duration(latencyThresholdMSec) * time.Millisecond)
		for _, name := range pluginsConcerned {
			if !common.StrInSlice(name, ctx.PluginNames()) {
				continue // ignore safely
			}

			rt, err := ctx.Statistics().PluginExecutionTimePercentile(
				name, pipelines.AllStatistics, 0.9) // value 90% is an option?
			if err != nil {
				logger.Warnf("[BUG: query plugin %s 90%% execution time failed, "+
					"ignored to adjust exceptional latency counter: %v]", pluginName, err)
				return
			}
			logger.Debugf("[concerned plugin %s latency: %.1f, latencyThreshold:%.1f]", name, rt, latencyThreshold)
			if rt < 0 {
				continue // doesn't make sense, defensive
			}

			latency += rt
			found = true
		}

		if !found {
			return
		}

		counter, err := getLatencyLimiterCounter(ctx, pluginName, allowMSec)
		if err != nil { // ignore error safely
			return
		}

		if latency < latencyThreshold {
			counter.Decrease()
		} else {
			counter.Increase()
		}
	}
}
