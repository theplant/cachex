package rediscachex

import (
	"context"
	"fmt"
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
	return &broadcast{client: cfg.Client, channel: channel}
}

type broadcast struct {
	client  redis.UniversalClient
	channel string
}

func (b *broadcast) Publish(ctx context.Context, msg []byte) error {
	if err := b.client.Publish(ctx, b.channel, msg).Err(); err != nil {
		return fmt.Errorf("rediscachex: publish to %s: %w", b.channel, err)
	}
	return nil
}

// Listen receives until ctx ends. go-redis reconnects a broken subscription
// on the next Receive and subscribes again, which answers with a
// *redis.Subscription: that is when ready runs.
func (b *broadcast) Listen(ctx context.Context, ready func(), deliver func(msg []byte)) error {
	ps := b.client.Subscribe(ctx, b.channel)
	stop := context.AfterFunc(ctx, func() { _ = ps.Close() }) // a blocked Receive ignores ctx's cancellation
	defer func() {
		stop()
		_ = ps.Close()
	}()
	backoff := 50 * time.Millisecond
	for {
		msg, err := ps.Receive(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
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
