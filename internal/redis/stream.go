package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// StreamMessage is one telemetry entry read from the events stream.
type StreamMessage struct {
	ID     string
	Fields map[string]string
}

// EnsureEventsGroup creates the consumer group that reads the telemetry stream, and
// the stream itself when it does not exist yet. An existing group is not an error, so
// a restarted supervisor resumes from where it left off.
func (c *Client) EnsureEventsGroup(ctx context.Context, group string) error {
	if group == "" {
		return errors.New("consumer group must not be empty")
	}
	err := c.rdb.XGroupCreateMkStream(ctx, c.keys.Events(), group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create consumer group %q on %s: %w", group, c.keys.Events(), err)
	}
	return nil
}

// ReadEventsGroup reads at most count entries for one consumer. id is "0" to replay
// this consumer's pending entries (what a crash left unacknowledged) and ">" for
// entries no consumer has seen. A negative block returns immediately; zero blocks.
func (c *Client) ReadEventsGroup(ctx context.Context, group, consumer, id string, count int64, block time.Duration) ([]StreamMessage, error) {
	streams, err := c.rdb.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{c.keys.Events(), id},
		Count:    count,
		Block:    block,
	}).Result()
	if errors.Is(err, goredis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read group %q from %s: %w", group, c.keys.Events(), err)
	}

	var messages []StreamMessage
	for _, stream := range streams {
		for _, message := range stream.Messages {
			fields := make(map[string]string, len(message.Values))
			for name, value := range message.Values {
				fields[name] = fmt.Sprint(value)
			}
			messages = append(messages, StreamMessage{ID: message.ID, Fields: fields})
		}
	}
	return messages, nil
}

// AckEvents marks entries as handled. An entry that is never acknowledged stays
// pending and is redelivered, so only consumed (or deliberately discarded) events are
// acknowledged.
func (c *Client) AckEvents(ctx context.Context, group string, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := c.rdb.XAck(ctx, c.keys.Events(), group, ids...).Err(); err != nil {
		return fmt.Errorf("ack %d entries in group %q: %w", len(ids), group, err)
	}
	return nil
}
