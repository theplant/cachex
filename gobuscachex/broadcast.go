// Package gobuscachex is the cachex invalidation broadcast over go-bus
// (github.com/qor5/go-bus), a publish-subscribe bus on PostgreSQL, which it
// needs: it is for deployments on PostgreSQL that have no Redis (with MySQL,
// use rediscachex.NewBroadcast). See cachex.WithBroadcast.
package gobuscachex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	bus "github.com/qor5/go-bus"

	"github.com/theplant/cachex/v2"
)

// DefaultTTL is how long a subscription lives without a heartbeat by
// default.
const DefaultTTL = time.Minute

// BroadcastConfig configures NewBroadcast.
type BroadcastConfig struct {
	// Bus is the bus. Required.
	Bus bus.Bus
	// Subject is the subject the instances of one cache publish to and
	// subscribe to: lowercase tokens separated by dots. Default
	// "cachex.invalidate"; give each cache its own when several share a bus.
	Subject string
	// TTL is how long an instance's subscription lives without a heartbeat;
	// it beats every third of it. Once it expired, what was published is
	// lost: the instance subscribes again, and the Cache clears its local
	// layers. It also bounds how long deliveries may stall unnoticed (see
	// NewBroadcast). Default DefaultTTL.
	TTL time.Duration
	// WorkerConfig tunes the consumer; its MaxLockPerSecond is how often it
	// polls the database, which bounds how soon a message arrives. Default
	// go-bus's.
	WorkerConfig *bus.WorkerConfig
	// Logger reports failed heartbeats. Default slog.Default().
	Logger *slog.Logger
}

// NewBroadcast returns a cachex.Broadcast over go-bus. Each Listen
// subscribes a queue of its own (with a random name) to the subject, with
// the TTL and auto-drain, keeps it alive by heartbeats, and unsubscribes it
// when its ctx ends. Messages wait in the queue while the instance is slow or
// briefly cut off; they are lost only once the subscription expired, which
// the next heartbeat finds: Listen then subscribes again and calls ready.
// The consumer polls the database, so a message arrives within about one
// polling interval.
//
// Listen also checks that deliveries go on: with each heartbeat it publishes a
// probe to a subject only its queue subscribes to, and if no probe arrived for
// a TTL it subscribes again (and calls ready). A consumer can stall while the
// heartbeats succeed: go-que's lock on the jobs lives on one connection, and
// once that connection dropped (a database restart or failover) the consumer
// fails for good.
func NewBroadcast(cfg BroadcastConfig) cachex.Broadcast {
	if cfg.Bus == nil {
		panic("gobuscachex: NewBroadcast: Bus is required")
	}
	if cfg.Subject == "" {
		cfg.Subject = "cachex.invalidate"
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &broadcast{cfg: cfg}
}

type broadcast struct{ cfg BroadcastConfig }

// Publish sends msg as the payload, which go-bus encodes as base64 JSON.
func (b *broadcast) Publish(ctx context.Context, msg []byte) error {
	if _, err := b.cfg.Bus.Publish(ctx, &bus.Outbound{Message: bus.Message{Subject: b.cfg.Subject, Payload: msg}}); err != nil {
		return fmt.Errorf("gobuscachex: publish to %s: %w", b.cfg.Subject, err)
	}
	return nil
}

func (b *broadcast) Listen(ctx context.Context, ready func(), deliver func(msg []byte)) error {
	var mu sync.Mutex // go-bus runs handlers concurrently; deliver takes one at a time
	handler := func(ctx context.Context, in *bus.Inbound) error {
		var msg []byte
		if err := json.Unmarshal(in.Payload, &msg); err != nil {
			return in.Destroy(ctx) // not ours: never retry it
		}
		mu.Lock()
		deliver(msg)
		mu.Unlock()
		return in.Destroy(ctx) // Done would keep the row for good
	}
	backoff := 100 * time.Millisecond
	for {
		err := b.subscribe(ctx, ready, handler)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.cfg.Logger.WarnContext(ctx, "gobuscachex: subscription lost, subscribing again", "subject", b.cfg.Subject, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}

// subscribe subscribes a new queue, calls ready, and consumes it until ctx
// ends, the subscription is found expired, or the probes stop arriving.
func (b *broadcast) subscribe(ctx context.Context, ready func(), handler bus.Handler) error {
	var id [8]byte
	_, _ = rand.Read(id[:])
	name := hex.EncodeToString(id[:])
	q := b.cfg.Bus.Queue("cachex-" + name)
	probe := "cachex.probe." + name // short: go-bus bounds a subject's tokens and length
	var subs []bus.Subscription
	defer func() { // not under ctx, which may be over
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		for _, sub := range subs {
			_ = sub.Unsubscribe(uctx)
		}
	}()
	for _, subject := range []string{b.cfg.Subject, probe} {
		sub, err := q.Subscribe(ctx, subject, bus.WithTTL(b.cfg.TTL), bus.WithAutoDrain(true))
		if err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}
		subs = append(subs, sub)
	}
	var probed atomic.Int64 // when a probe last arrived, or when this began
	probed.Store(time.Now().UnixNano())
	inner := handler
	handler = func(ctx context.Context, in *bus.Inbound) error {
		if in.Subject == probe {
			probed.Store(time.Now().UnixNano())
			return in.Destroy(ctx)
		}
		return inner(ctx, in)
	}
	ready()
	var opts []bus.ConsumeOption
	if b.cfg.WorkerConfig != nil {
		opts = append(opts, bus.WithWorkerConfig(b.cfg.WorkerConfig))
	}
	consumer, err := q.StartConsumer(ctx, handler, opts...)
	if err != nil {
		return fmt.Errorf("start consumer: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = consumer.Stop(sctx)
	}()
	tick := time.NewTicker(b.cfg.TTL / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-consumer.Done():
			return fmt.Errorf("consumer stopped: %w", consumer.Err())
		case <-tick.C:
			for _, sub := range subs {
				err := sub.Heartbeat(ctx)
				switch {
				case errors.Is(err, bus.ErrSubscriptionNotFound):
					return err // expired: messages may have been lost
				case err != nil && ctx.Err() == nil: // the database may be down: try again on the next tick
					b.cfg.Logger.WarnContext(ctx, "gobuscachex: heartbeat failed", "subject", b.cfg.Subject, "error", err)
				}
			}
			if time.Since(time.Unix(0, probed.Load())) > b.cfg.TTL {
				return errors.New("deliveries stalled: no probe arrived for a TTL")
			}
			if _, err := b.cfg.Bus.Publish(ctx, &bus.Outbound{Message: bus.Message{Subject: probe}}); err != nil && ctx.Err() == nil {
				b.cfg.Logger.WarnContext(ctx, "gobuscachex: probe failed", "subject", probe, "error", err)
			}
		}
	}
}
