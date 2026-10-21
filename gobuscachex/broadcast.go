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
	"time"

	bus "github.com/qor5/go-bus"
	"github.com/qor5/go-bus/bussql"

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
	// subscribe to: up to 16 tokens of lowercase letters, digits, '_' and '-',
	// separated by dots (NewBroadcast panics on another). Default
	// "cachex.invalidate"; give each cache its own when several share a bus.
	Subject string
	// TTL is how long an instance's subscription lives without a heartbeat;
	// it beats every third of it. Once it expired, what was published is
	// lost: the instance subscribes again, and the Cache clears its local
	// layers. A whole number of microseconds, as go-bus stores it
	// (NewBroadcast panics on another). Default DefaultTTL.
	TTL time.Duration
	// WorkerConfig tunes the consumer; its MaxLockPerSecond is how often it
	// polls the database, which bounds how soon a message arrives. Default
	// go-bus's.
	WorkerConfig *bus.WorkerConfig
	// Logger reports failed heartbeats, lost subscriptions and failures to
	// unsubscribe the expired subscriptions of dead instances. Default
	// slog.Default().
	Logger *slog.Logger
}

// NewBroadcast returns a cachex.Broadcast over go-bus. Each Listen
// subscribes a queue of its own (with a random name) to the subject, with
// the TTL and auto-drain, keeps it alive by heartbeats, and unsubscribes it
// when its ctx ends. Messages wait in the queue while the instance is slow or
// briefly cut off; they are lost only once the subscription expired, which
// the next heartbeat finds: Listen then subscribes again and calls ready.
// The consumer polls the database, so a message arrives within about one
// polling interval. While the database is unreachable nothing arrives; once it
// is back, deliveries resume after the consumer's reconnect backoff, up to
// about 45s with go-bus's default (30s at most, randomized), and the messages
// published meanwhile arrive then. Local layers may serve values written
// elsewhere during that time; only an expired subscription clears them. With each heartbeat, Listen also unsubscribes the expired
// subscriptions of instances that died without unsubscribing, draining their
// queues, a bounded batch at a time.
func NewBroadcast(cfg BroadcastConfig) cachex.Broadcast {
	if cfg.Bus == nil {
		panic("gobuscachex: NewBroadcast: Bus is required")
	}
	if cfg.Subject == "" {
		cfg.Subject = "cachex.invalidate"
	}
	if err := bus.ValidateSubject(cfg.Subject); err != nil { // a subject is also a pattern to subscribe to
		panic("gobuscachex: NewBroadcast: " + err.Error())
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.TTL%time.Microsecond != 0 { // go-bus stores it in microseconds, and rejects it otherwise
		panic("gobuscachex: NewBroadcast: TTL must be a whole number of microseconds")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &broadcast{cfg: cfg}
}

type broadcast struct{ cfg BroadcastConfig }

// Publish sends msg as the payload, which go-bus encodes as base64 JSON.
func (b *broadcast) Publish(ctx context.Context, msg []byte) error {
	// never in the caller's transaction (go-bus takes one from ctx): it may
	// have ended, or roll back, and the message is the cache's, not the caller's
	ctx = bussql.NewContext(ctx, nil)
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
		defer mu.Unlock() // go-que recovers a panic in a handler: the lock must not stay held
		deliver(msg)
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
// ends or the subscription is found expired. With each heartbeat it also
// unsubscribes the expired subscriptions of instances that died without
// unsubscribing (see expiredUnsubscriber).
func (b *broadcast) subscribe(ctx context.Context, ready func(), handler bus.Handler) error {
	var id [8]byte
	_, _ = rand.Read(id[:])
	q := b.cfg.Bus.Queue("cachex-" + hex.EncodeToString(id[:]))
	sub, err := q.Subscribe(ctx, b.cfg.Subject, bus.WithTTL(b.cfg.TTL), bus.WithAutoDrain(true))
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer func() { // not under ctx, which may be over
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = sub.Unsubscribe(uctx)
	}()
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
			err := sub.Heartbeat(ctx)
			switch {
			case errors.Is(err, bus.ErrSubscriptionNotFound):
				return err // expired: messages may have been lost
			case err != nil && ctx.Err() == nil: // the database may be down: try again on the next tick
				b.cfg.Logger.WarnContext(ctx, "gobuscachex: heartbeat failed", "subject", b.cfg.Subject, "error", err)
			}
			if u, ok := b.cfg.Bus.(expiredUnsubscriber); ok {
				if _, err := u.UnsubscribeExpired(ctx); err != nil && ctx.Err() == nil {
					b.cfg.Logger.WarnContext(ctx, "gobuscachex: unsubscribing expired subscriptions failed", "error", err)
				}
			}
		}
	}
}

// expiredUnsubscriber is a bus that unsubscribes, on their owners' behalf,
// subscriptions whose TTL ran out (go-bus's BusImpl): an instance that died
// without unsubscribing (SIGKILL, OOM) would leave its subscription and its
// queue's messages behind for good. It handles a bounded batch a call.
type expiredUnsubscriber interface {
	UnsubscribeExpired(ctx context.Context) (int, error)
}
