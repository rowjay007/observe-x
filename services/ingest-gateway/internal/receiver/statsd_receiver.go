package receiver

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rowjay007/observe-x/pkg/engine"
	"github.com/rowjay007/observe-x/pkg/signal"
	"go.uber.org/zap"
)

const (
	// statsdMaxPacketSize is the maximum UDP packet size for StatsD.
	// Standard StatsD implementations use 8932 bytes (MTU-safe).
	statsdMaxPacketSize = 8932

	// statsdReadBufferSize sets the OS-level UDP socket receive buffer.
	statsdReadBufferSize = 4 * 1024 * 1024 // 4 MB
)

// StatsDReceiver listens for StatsD-formatted UDP packets and converts
// them into ObserveX signals. It supports the standard StatsD wire format:
//
//	<metric_name>:<value>|<type>[|@<sample_rate>][|#<tag1>:<val1>,<tag2>:<val2>]
//
// Supported metric types: c (counter), g (gauge), ms (timer), h (histogram), s (set).
type StatsDReceiver struct {
	addr       string
	conn       *net.UDPConn
	engine     *engine.ProcessingEngine
	tenantID   string // default tenant for unauthenticated StatsD
	logger     *zap.Logger
	mu         sync.Mutex
	running    bool
	cancelFunc context.CancelFunc
}

// NewStatsDReceiver creates a new StatsD receiver that listens on the given
// UDP address (e.g. ":8125") and routes parsed metrics to the processing engine.
func NewStatsDReceiver(addr string, eng *engine.ProcessingEngine, defaultTenantID string, logger *zap.Logger) *StatsDReceiver {
	return &StatsDReceiver{
		addr:     addr,
		engine:   eng,
		tenantID: defaultTenantID,
		logger:   logger,
	}
}

// Start begins listening for StatsD UDP packets. It blocks until the context
// is cancelled or Stop is called.
func (r *StatsDReceiver) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("StatsD receiver already running")
	}

	udpAddr, err := net.ResolveUDPAddr("udp", r.addr)
	if err != nil {
		r.mu.Unlock()
		return fmt.Errorf("failed to resolve UDP address %s: %w", r.addr, err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		r.mu.Unlock()
		return fmt.Errorf("failed to listen on UDP %s: %w", r.addr, err)
	}

	// Increase the socket receive buffer to handle bursts
	if err := conn.SetReadBuffer(statsdReadBufferSize); err != nil {
		r.logger.Warn("failed to set UDP read buffer size", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(ctx)
	r.conn = conn
	r.cancelFunc = cancel
	r.running = true
	r.mu.Unlock()

	r.logger.Info("StatsD receiver started", zap.String("addr", r.addr))

	buf := make([]byte, statsdMaxPacketSize)
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("StatsD receiver shutting down")
			return nil
		default:
		}

		// Set a short read deadline so we can check context cancellation
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))

		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			// If we're shutting down, this is expected
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			r.logger.Warn("StatsD read error", zap.Error(err))
			continue
		}

		if n == 0 {
			continue
		}

		r.processPacket(ctx, buf[:n])
	}
}

// Stop shuts down the StatsD receiver.
func (r *StatsDReceiver) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelFunc != nil {
		r.cancelFunc()
	}
	if r.conn != nil {
		_ = r.conn.Close()
		r.running = false
	}
}

// processPacket parses a raw StatsD UDP packet. A single packet may contain
// multiple metrics separated by newlines.
func (r *StatsDReceiver) processPacket(ctx context.Context, data []byte) {
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		metric, err := parseStatsDLine(string(line))
		if err != nil {
			r.logger.Debug("failed to parse StatsD line",
				zap.String("line", string(line)),
				zap.Error(err),
			)
			continue
		}

		attrs := map[string]string{
			"metric_name": metric.Name,
			"metric_type": metric.MetricType,
			"source":      "statsd",
		}

		// Copy tags into attributes
		for k, v := range metric.Tags {
			attrs["tag."+k] = v
		}

		if metric.SampleRate > 0 && metric.SampleRate < 1 {
			attrs["sample_rate"] = strconv.FormatFloat(metric.SampleRate, 'f', -1, 64)
		}

		payload := []byte(fmt.Sprintf(`{"name":"%s","value":%s,"type":"%s"}`,
			metric.Name, metric.Value, metric.MetricType))

		sig := signal.Signal{
			TenantID:   r.tenantID,
			Type:       signal.Metric,
			Payload:    payload,
			Attributes: attrs,
			ReceivedAt: time.Now(),
		}

		if err := r.engine.ProcessSignal(ctx, sig); err != nil {
			r.logger.Warn("failed to process StatsD metric",
				zap.String("metric", metric.Name),
				zap.Error(err),
			)
		}
	}
}

// statsdMetric holds a parsed StatsD metric.
type statsdMetric struct {
	Name       string
	Value      string
	MetricType string
	SampleRate float64
	Tags       map[string]string
}

// parseStatsDLine parses a single StatsD line in the format:
//
//	<metric_name>:<value>|<type>[|@<sample_rate>][|#<tag1>:<val1>,<tag2>:<val2>]
func parseStatsDLine(line string) (*statsdMetric, error) {
	// Split name from the rest: "metric.name:value|type|..."
	colonIdx := strings.IndexByte(line, ':')
	if colonIdx < 1 {
		return nil, fmt.Errorf("invalid statsd format: missing colon separator")
	}

	name := line[:colonIdx]
	rest := line[colonIdx+1:]

	// Split by pipe: "value|type[|@rate][|#tags]"
	parts := strings.Split(rest, "|")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid statsd format: missing type separator")
	}

	value := parts[0]
	metricType := parts[1]

	// Validate metric type
	switch metricType {
	case "c", "g", "ms", "h", "s":
		// valid
	default:
		return nil, fmt.Errorf("unknown metric type: %s", metricType)
	}

	m := &statsdMetric{
		Name:       name,
		Value:      value,
		MetricType: metricType,
		Tags:       make(map[string]string),
	}

	// Parse optional fields: sample rate and tags
	for _, part := range parts[2:] {
		if strings.HasPrefix(part, "@") {
			rate, err := strconv.ParseFloat(part[1:], 64)
			if err == nil {
				m.SampleRate = rate
			}
		} else if strings.HasPrefix(part, "#") {
			tagStr := part[1:]
			tagPairs := strings.Split(tagStr, ",")
			for _, pair := range tagPairs {
				kv := strings.SplitN(pair, ":", 2)
				if len(kv) == 2 {
					m.Tags[kv[0]] = kv[1]
				} else if len(kv) == 1 {
					m.Tags[kv[0]] = ""
				}
			}
		}
	}

	return m, nil
}
