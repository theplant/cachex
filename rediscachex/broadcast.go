package rediscachex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/theplant/cachex/v2"
)

// BroadcastConfig configures NewBroadcast.
type BroadcastConfig struct {
	// Client is the Redis client. Required.
	Client redis.UniversalClient
	// KeyPrefix is the KeyPrefix of the cache's Redis layer, if it has one:
	// the instances of one cache share it, so the channel is
	// "cachex-invalidate:" followed by it.
	KeyPrefix string
	// Channel overrides the channel.
	Channel string
}

// NewBroadcast returns a cachex.Broadcast over Redis Pub/Sub (on Redis
// Cluster too, where PUBLISH reaches every node). Messages are pushed at
// once, but those published while a subscription is down are lost: on each
// (re)subscription Listen calls ready, and the Cache clears its local layers
// (see cachex.WithBroadcast).
func NewBroadcast(cfg BroadcastConfig) cachex.Broadcast {
	if cfg.Client == nil {
		panic("rediscachex: NewBroadcast: Client is required")
	}
	channel := cfg.Channel
	if channel == "" {
		channel = "cachex-invalidate:" + cfg.KeyPrefix
	}
	return &broadcast{client: cfg.Client, channel: channel, healthCheck: healthCheck}
}

type broadcast struct {
	client      redis.UniversalClient
	channel     string
	healthCheck time.Duration
}

func (b *broadcast) Publish(ctx context.Context, msg []byte) error {
	if err := b.client.Publish(ctx, b.channel, msg).Err(); err != nil {
		return fmt.Errorf("rediscachex: publish to %s: %w", b.channel, err)
	}
	return nil
}

// healthCheck is how long Listen waits for anything on the subscription
// before it pings; as long again without an answer, and it subscribes anew.
// NewBroadcast reads it; tests shorten it.
var healthCheck = 15 * time.Second

// errDead reports a subscription that stopped answering pings.
var errDead = errors.New("rediscachex: the subscription stopped answering")

// Listen receives until ctx ends. go-redis reconnects a broken subscription
// on the next Receive and subscribes again, which answers with a
// *redis.Subscription: that is when ready runs. A connection can also die
// without a word (a failed-over host, a NAT that forgot it): Receive would
// wait for good, so Listen pings when the subscription is quiet, and
// subscribes anew when no answer comes.
func (b *broadcast) Listen(ctx context.Context, ready func(), deliver func(msg []byte)) error {
	for {
		if err := b.listen(ctx, ready, deliver); ctx.Err() != nil {
			return ctx.Err()
		} else if !errors.Is(err, errDead) {
			return err
		}
	}
}

// listen is Listen on one subscription, until ctx ends or it is found dead.
func (b *broadcast) listen(ctx context.Context, ready func(), deliver func(msg []byte)) error {
	ps := b.client.Subscribe(ctx, b.channel)
	stop := context.AfterFunc(ctx, func() { _ = ps.Close() }) // a blocked Receive ignores ctx's cancellation
	defer func() {
		stop()
		_ = ps.Close()
	}()
	backoff := 50 * time.Millisecond
	pinged := false
	for {
		msg, err := ps.ReceiveTimeout(ctx, b.healthCheck)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			if pinged {
				return errDead
			}
			pinged = true
			_ = ps.Ping(ctx) // its answer, or its failure, comes through Receive
			continue
		}
		if err != nil { // reconnected by the next Receive; do not spin while Redis is down
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 2*time.Second)
			continue
		}
		backoff = 50 * time.Millisecond
		pinged = false
		switch m := msg.(type) {
		case *redis.Subscription:
			if m.Kind == "subscribe" {
				ready()
			}
		case *redis.Message:
			deliver([]byte(m.Payload))
		}
	}
}
