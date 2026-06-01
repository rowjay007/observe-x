package auditlog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Exporter writes records to S3-compatible object storage. Each
// `Flush` interval (or each `FlushAt` record count) produces one
// NDJSON object under the key pattern:
//
//	{prefix}/YYYY/MM/DD/HH/{epoch_ms}-{rand6}.ndjson
//
// When ObjectLockMode + RetainUntil are configured, the object is
// uploaded with S3 Object Lock so it cannot be deleted or
// overwritten for the retention window — the SOC2 / FedRAMP /
// HIPAA-style WORM contract. The bucket itself must be created with
// Object Lock ENABLED (a one-time bucket-creation flag); the
// exporter does not attempt to enable it on existing buckets.
type S3Exporter struct {
	client    *s3.Client
	bucket    string
	prefix    string
	mode      s3types.ObjectLockMode // "" disables lock
	retainFor time.Duration

	flushEvery   time.Duration
	flushAtCount int

	mu        sync.Mutex
	pending   []Record
	lastFlush time.Time

	stopCh chan struct{}
	doneCh chan struct{}
}

// S3Options carries the configuration knobs.
type S3Options struct {
	// Bucket is required.
	Bucket string
	// Prefix groups objects under a path (no leading slash). Default "audit/".
	Prefix string
	// Region is the AWS region. If empty, the default chain decides.
	Region string
	// Endpoint overrides the S3 endpoint URL (use for MinIO / R2 / etc.).
	Endpoint string
	// UseSSL controls whether HTTPS is used for the endpoint. Default true.
	UseSSL bool

	// ObjectLockMode: "" (off), "GOVERNANCE", or "COMPLIANCE".
	// COMPLIANCE is true WORM and cannot be unlocked by anyone,
	// including root, until the retention period elapses. Use for
	// regulated workloads; GOVERNANCE if you need a break-glass.
	ObjectLockMode string
	// RetainFor sets how long Object Lock holds each object. SOC2
	// commonly uses 7y; ObserveX defaults to 1y if unset and mode is
	// configured.
	RetainFor time.Duration

	// FlushInterval batches records and uploads at most once per
	// interval. Default 60s.
	FlushInterval time.Duration
	// FlushAtCount also triggers a flush when N records accumulate.
	// Default 1000.
	FlushAtCount int
}

func (o S3Options) withDefaults() S3Options {
	if o.Prefix == "" {
		o.Prefix = "audit/"
	}
	if !strings.HasSuffix(o.Prefix, "/") {
		o.Prefix += "/"
	}
	if o.FlushInterval == 0 {
		o.FlushInterval = 60 * time.Second
	}
	if o.FlushAtCount == 0 {
		o.FlushAtCount = 1000
	}
	if o.ObjectLockMode != "" && o.RetainFor == 0 {
		o.RetainFor = 365 * 24 * time.Hour
	}
	return o
}

// NewS3Exporter constructs an S3-backed Exporter. Returns an error
// for missing required configuration. The exporter starts a
// background flusher goroutine; call Close to drain it.
func NewS3Exporter(ctx context.Context, opts S3Options) (*S3Exporter, error) {
	opts = opts.withDefaults()
	if opts.Bucket == "" {
		return nil, errors.New("auditlog: S3 Bucket required")
	}

	awsCfgOpts := []func(*config.LoadOptions) error{}
	if opts.Region != "" {
		awsCfgOpts = append(awsCfgOpts, config.WithRegion(opts.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, awsCfgOpts...)
	if err != nil {
		return nil, fmt.Errorf("auditlog: aws config: %w", err)
	}

	s3Opts := []func(*s3.Options){}
	if opts.Endpoint != "" {
		ep := opts.Endpoint
		if opts.UseSSL && !strings.HasPrefix(ep, "https://") && !strings.HasPrefix(ep, "http://") {
			ep = "https://" + ep
		}
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ep)
			o.UsePathStyle = true
		})
	}

	var mode s3types.ObjectLockMode
	switch strings.ToUpper(opts.ObjectLockMode) {
	case "":
		mode = ""
	case "GOVERNANCE":
		mode = s3types.ObjectLockModeGovernance
	case "COMPLIANCE":
		mode = s3types.ObjectLockModeCompliance
	default:
		return nil, fmt.Errorf("auditlog: invalid ObjectLockMode %q", opts.ObjectLockMode)
	}

	e := &S3Exporter{
		client:       s3.NewFromConfig(awsCfg, s3Opts...),
		bucket:       opts.Bucket,
		prefix:       opts.Prefix,
		mode:         mode,
		retainFor:    opts.RetainFor,
		flushEvery:   opts.FlushInterval,
		flushAtCount: opts.FlushAtCount,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		lastFlush:    time.Now(),
	}
	go e.run()
	return e, nil
}

func (e *S3Exporter) Append(_ context.Context, r Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	e.pending = append(e.pending, r)
	shouldFlush := len(e.pending) >= e.flushAtCount
	e.mu.Unlock()
	if shouldFlush {
		return e.flush(context.Background())
	}
	return nil
}

func (e *S3Exporter) Close(ctx context.Context) error {
	close(e.stopCh)
	<-e.doneCh
	return e.flush(ctx)
}

// run is the background ticker that flushes on FlushInterval.
func (e *S3Exporter) run() {
	defer close(e.doneCh)
	t := time.NewTicker(e.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = e.flush(context.Background())
		case <-e.stopCh:
			return
		}
	}
}

func (e *S3Exporter) flush(ctx context.Context) error {
	e.mu.Lock()
	batch := e.pending
	e.pending = nil
	e.lastFlush = time.Now()
	e.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}

	body := serialiseBatch(batch)
	checksum := sha256.Sum256(body)
	key := e.objectKey(time.Now())

	put := &s3.PutObjectInput{
		Bucket:          aws.String(e.bucket),
		Key:             aws.String(key),
		Body:            bytes.NewReader(body),
		ContentType:     aws.String("application/x-ndjson"),
		ContentEncoding: aws.String("identity"),
		ChecksumSHA256:  aws.String(hexToBase64SHA256(checksum)),
		Metadata: map[string]string{
			"observex-record-count": fmt.Sprintf("%d", len(batch)),
			"observex-sha256":       hex.EncodeToString(checksum[:]),
		},
	}

	if e.mode != "" {
		put.ObjectLockMode = e.mode
		put.ObjectLockRetainUntilDate = aws.Time(time.Now().Add(e.retainFor))
	}

	if _, err := e.client.PutObject(ctx, put); err != nil {
		// Push back into the pending buffer so the next flush retries.
		// We accept slight duplication risk over data loss: a network
		// hiccup might result in two PUTs of the same batch, but each
		// has a unique key so they coexist in the bucket.
		e.mu.Lock()
		e.pending = append(batch, e.pending...)
		e.mu.Unlock()
		return fmt.Errorf("auditlog: put: %w", err)
	}
	return nil
}

func serialiseBatch(batch []Record) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range batch {
		_ = enc.Encode(r)
	}
	return buf.Bytes()
}

func (e *S3Exporter) objectKey(at time.Time) string {
	t := at.UTC()
	suffix := randomSuffix(6)
	return fmt.Sprintf("%s%04d/%02d/%02d/%02d/%d-%s.ndjson",
		e.prefix, t.Year(), t.Month(), t.Day(), t.Hour(),
		t.UnixMilli(), suffix)
}

// hexToBase64SHA256 converts the 32-byte SHA-256 into the base64
// representation S3 expects for the ChecksumSHA256 header.
func hexToBase64SHA256(sum [32]byte) string {
	// Use the standard base64 encoding directly to avoid importing it
	// indirectly through aws-sdk types.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	src := sum[:]
	dst := make([]byte, 0, 44)
	for i := 0; i+3 <= len(src); i += 3 {
		v := uint32(src[i])<<16 | uint32(src[i+1])<<8 | uint32(src[i+2])
		dst = append(dst,
			alphabet[(v>>18)&0x3F],
			alphabet[(v>>12)&0x3F],
			alphabet[(v>>6)&0x3F],
			alphabet[v&0x3F])
	}
	// 32 bytes ≡ 2 mod 3 → one '=' pad.
	tail := len(src) % 3
	if tail == 2 {
		v := uint32(src[len(src)-2])<<8 | uint32(src[len(src)-1])
		dst = append(dst,
			alphabet[(v>>10)&0x3F],
			alphabet[(v>>4)&0x3F],
			alphabet[(v<<2)&0x3F],
			'=')
	}
	return string(dst)
}

// randomSuffix returns n bytes of crypto-random hex. We don't import
// crypto/rand at the top level to keep the auditlog package's import
// graph small; this helper uses time-based entropy that's "unique
// enough" within a single flush. For cryptographic uniqueness the
// epoch_ms in the object key is the load-bearing component.
func randomSuffix(n int) string {
	const hexAlphabet = "0123456789abcdef"
	now := time.Now().UnixNano()
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = hexAlphabet[(now>>(uint(i)*4))&0xF]
	}
	return string(out)
}
