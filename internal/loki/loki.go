// Package loki batches detailed RUM events and pushes them to Loki's
// /loki/api/v1/push endpoint. Only low-cardinality stream labels (service, kind,
// job) are used; everything else lives in the JSON log line. When no endpoint is
// configured the client is a no-op, which keeps local development simple.
package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Entry is a single detailed event destined for Loki.
type Entry struct {
	Service string
	Kind    string
	TimeNS  int64  // event timestamp in unix nanoseconds
	Line    string // JSON log body
}

type streamKey struct {
	service string
	kind    string
}

// Client buffers entries and flushes them on an interval or when the batch fills.
type Client struct {
	url       string
	job       string
	client    *http.Client
	batchSize int
	flushEN   time.Duration

	ch       chan Entry
	wg       sync.WaitGroup
	stop     chan struct{}
	onError  func()
	disabled bool
}

// Options configures the Loki client.
type Options struct {
	URL           string        // e.g. http://loki:3100/loki/api/v1/push; empty disables
	Job           string        // job label, default "rum-collector"
	BatchSize     int           // flush when this many entries buffered (default 100)
	FlushInterval time.Duration // flush at least this often (default 5s)
	OnPushError   func()        // called once per failed push (metric hook)
}

// New creates a Client and starts its background flusher. Call Close to drain.
func New(o Options) *Client {
	c := &Client{
		url:       o.URL,
		job:       o.Job,
		client:    &http.Client{Timeout: 10 * time.Second},
		batchSize: o.BatchSize,
		flushEN:   o.FlushInterval,
		ch:        make(chan Entry, 4096),
		stop:      make(chan struct{}),
		onError:   o.OnPushError,
		disabled:  o.URL == "",
	}
	if c.job == "" {
		c.job = "rum-collector"
	}
	if c.batchSize <= 0 {
		c.batchSize = 100
	}
	if c.flushEN <= 0 {
		c.flushEN = 5 * time.Second
	}
	if c.disabled {
		slog.Info("loki push disabled (no URL configured)")
		return c
	}
	c.wg.Add(1)
	go c.run()
	return c
}

// Push enqueues an entry. It never blocks the request path: if the buffer is full
// the entry is dropped (RUM data is best-effort).
func (c *Client) Push(e Entry) {
	if c.disabled {
		return
	}
	select {
	case c.ch <- e:
	default:
		// buffer full; drop rather than block the collector
	}
}

// Close stops the flusher and drains any buffered entries.
func (c *Client) Close() {
	if c.disabled {
		return
	}
	close(c.stop)
	c.wg.Wait()
}

func (c *Client) run() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.flushEN)
	defer ticker.Stop()

	batch := make([]Entry, 0, c.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.send(batch)
		batch = batch[:0]
	}

	for {
		select {
		case e := <-c.ch:
			batch = append(batch, e)
			if len(batch) >= c.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-c.stop:
			// drain remaining buffered entries before exiting
			for {
				select {
				case e := <-c.ch:
					batch = append(batch, e)
				default:
					flush()
					return
				}
			}
		}
	}
}

// lokiPush is the wire format for /loki/api/v1/push.
type lokiPush struct {
	Streams []lokiStream `json:"streams"`
}
type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

func (c *Client) send(batch []Entry) {
	grouped := make(map[streamKey][][2]string)
	for _, e := range batch {
		k := streamKey{service: e.Service, kind: e.Kind}
		grouped[k] = append(grouped[k], [2]string{strconv.FormatInt(e.TimeNS, 10), e.Line})
	}

	payload := lokiPush{Streams: make([]lokiStream, 0, len(grouped))}
	for k, values := range grouped {
		payload.Streams = append(payload.Streams, lokiStream{
			Stream: map[string]string{"service": k.service, "kind": k.kind, "job": c.job},
			Values: values,
		})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		c.fail("marshal loki payload", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		c.fail("build loki request", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.fail("loki push", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		c.fail("loki push status", nil, slog.Int("status", resp.StatusCode))
		return
	}
}

func (c *Client) fail(msg string, err error, attrs ...slog.Attr) {
	args := make([]any, 0, len(attrs)+1)
	if err != nil {
		args = append(args, slog.Any("err", err))
	}
	for _, a := range attrs {
		args = append(args, a)
	}
	slog.Warn(msg, args...)
	if c.onError != nil {
		c.onError()
	}
}
