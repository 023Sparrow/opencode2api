package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type proxyTransport struct {
	name   string
	client *http.Client
}

type transportPool struct {
	items []proxyTransport
}

func newTransportPool(proxies []string, cfg PerformanceConfig, responseHeaderTimeout time.Duration) (*transportPool, error) {
	p := &transportPool{items: make([]proxyTransport, 0, len(proxies))}
	for _, raw := range proxies {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = cfg.MaxIdleConns
		transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
		transport.MaxConnsPerHost = cfg.MaxConnsPerHost
		transport.IdleConnTimeout = time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		transport.ForceAttemptHTTP2 = true
		transport.DialContext = (&net.Dialer{
			Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		if raw == "direct" {
			transport.Proxy = nil
		} else {
			u, err := url.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("parse proxy %s: %w", redactURL(raw), err)
			}
			transport.Proxy = http.ProxyURL(u)
		}
		p.items = append(p.items, proxyTransport{name: raw, client: &http.Client{Transport: transport}})
	}
	return p, nil
}

// upstreamNode is immutable except for health counters. A key is permanently
// assigned to one proxy, so the request hot path never parses URLs or builds a
// key/proxy combination.
type upstreamNode struct {
	key           string
	proxy         string
	client        *http.Client
	index         int
	failures      atomic.Uint32
	cooldownUntil atomic.Int64
}

type nodePool struct {
	nodes    []*upstreamNode
	next     atomic.Uint64
	cooldown time.Duration
}

// newNodePool distributes N keys over M proxies in round-robin order. N/M must
// be a positive integer, so every proxy receives exactly the same key count.
func newNodePool(keys []string, transports *transportPool, cooldown time.Duration) (*nodePool, error) {
	if len(keys) == 0 {
		return &nodePool{cooldown: cooldown}, nil
	}
	if len(transports.items) > len(keys) {
		return nil, fmt.Errorf("cannot evenly assign %d keys to %d proxies: every proxy must receive at least one key", len(keys), len(transports.items))
	}
	if len(keys)%len(transports.items) != 0 {
		return nil, fmt.Errorf("cannot evenly assign %d keys to %d proxies: key count must be divisible by proxy count", len(keys), len(transports.items))
	}
	pool := &nodePool{nodes: make([]*upstreamNode, 0, len(keys)), cooldown: cooldown}
	for i, key := range keys {
		proxy := transports.items[i%len(transports.items)]
		pool.nodes = append(pool.nodes, &upstreamNode{key: key, proxy: proxy.name, client: proxy.client, index: i})
	}
	return pool, nil
}

func (p *nodePool) Len() int { return len(p.nodes) }

type nodeCursor struct {
	pool *nodePool
	next int
}

// Cursor reserves a different starting node for each concurrent request.
// Selection is delayed until Next, so a node marked failed by the preceding
// attempt is immediately skipped. Both Cursor and Next allocate no memory.
func (p *nodePool) Cursor() nodeCursor {
	if len(p.nodes) == 0 {
		return nodeCursor{pool: p}
	}
	return nodeCursor{pool: p, next: int((p.next.Add(1) - 1) % uint64(len(p.nodes)))}
}

func (c *nodeCursor) Next() *upstreamNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	choice := -1
	var earliest int64
	for offset := 0; offset < len(c.pool.nodes); offset++ {
		i := (c.next + offset) % len(c.pool.nodes)
		until := c.pool.nodes[i].cooldownUntil.Load()
		if until <= now {
			choice = i
			break
		}
		if choice == -1 || until < earliest {
			choice, earliest = i, until
		}
	}
	if choice < 0 {
		return nil
	}
	c.next = (choice + 1) % len(c.pool.nodes)
	return c.pool.nodes[choice]
}

func (p *nodePool) MarkSuccess(node *upstreamNode) {
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

func (p *nodePool) MarkFailure(node *upstreamNode, resp *http.Response, err error) {
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	multiplier := time.Duration(1 << min(failures-1, 3))
	delay := p.cooldown * multiplier
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = retryAfter
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
