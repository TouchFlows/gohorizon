package redis

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Config is the connection half of redis.* in gohorizon.yaml.
type Config struct {
	Address     string
	Username    string
	Password    string
	DB          int
	TLS         bool
	DialTimeout time.Duration
}

// Client is a namespaced Redis handle. It deliberately exposes no generic command
// method: the only operations GoHorizon needs are lock operations, telemetry
// publishing and telemetry consumption, each with its own typed method.
type Client struct {
	rdb  *goredis.Client
	keys Keys
}

// Open connects and pings. Startup requires Redis because the ownership lock does
// (docs/adr/0004-ownership-lock.md).
func Open(ctx context.Context, cfg Config, keys Keys) (*Client, error) {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 3 * time.Second
	}
	opts := &goredis.Options{
		Addr:         cfg.Address,
		Username:     cfg.Username,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	if cfg.TLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	rdb := goredis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("connect to redis at %s: %w", cfg.Address, err)
	}
	return &Client{rdb: rdb, keys: keys}, nil
}

func (c *Client) Keys() Keys { return c.keys }

func (c *Client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

func (c *Client) Close() error { return c.rdb.Close() }

var releaseScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

var renewScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// AcquireSupervisorLock takes the single-instance ownership lock. It returns false
// (no error) when another owner holds it.
func (c *Client) AcquireSupervisorLock(ctx context.Context, owner string, ttl time.Duration) (bool, error) {
	if owner == "" {
		return false, fmt.Errorf("lock owner must not be empty")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("lock ttl must be positive")
	}
	ok, err := c.rdb.SetNX(ctx, c.keys.SupervisorLock(), owner, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("acquire supervisor lock: %w", err)
	}
	return ok, nil
}

// RenewSupervisorLock extends the lock only if this owner still holds it, so a
// stalled process cannot renew someone else's lock.
func (c *Client) RenewSupervisorLock(ctx context.Context, owner string, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, c.rdb, []string{c.keys.SupervisorLock()}, owner, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("renew supervisor lock: %w", err)
	}
	return n == 1, nil
}

func (c *Client) ReleaseSupervisorLock(ctx context.Context, owner string) error {
	if _, err := releaseScript.Run(ctx, c.rdb, []string{c.keys.SupervisorLock()}, owner).Result(); err != nil {
		return fmt.Errorf("release supervisor lock: %w", err)
	}
	return nil
}

// SupervisorLockHolder returns the current owner token, or "" when unlocked.
func (c *Client) SupervisorLockHolder(ctx context.Context) (string, error) {
	v, err := c.rdb.Get(ctx, c.keys.SupervisorLock()).Result()
	if err == goredis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read supervisor lock: %w", err)
	}
	return v, nil
}
