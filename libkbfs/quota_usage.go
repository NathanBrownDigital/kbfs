// Copyright 2017 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/keybase/client/go/logger"
	"github.com/keybase/kbfs/kbfsblock"
	"golang.org/x/net/context"
)

// ECQUCtxTagKey is the type for unique ECQU background opertaion IDs.
type ECQUCtxTagKey struct{}

// ECQUID is used in EventuallyConsistentQuotaUsage for only background RPCs.
// More specifically, when we need to spawn a background goroutine for
// GetUserQuotaInfo, a new context with this tag is created and used. This is
// also used as a prefix for the logger module name in
// EventuallyConsistentQuotaUsage.
const ECQUID = "ECQU"

type cachedQuotaUsage struct {
	timestamp  time.Time
	usageBytes int64
	limitBytes int64
}

// EventuallyConsistentQuotaUsage keeps tracks of quota usage, in a way user of
// which can choose to accept stale data to reduce calls into block servers.
type EventuallyConsistentQuotaUsage struct {
	config Config
	log    logger.Logger

	backgroundInProcess int32

	mu     sync.RWMutex
	cached cachedQuotaUsage
}

// NewEventuallyConsistentQuotaUsage creates a new
// EventuallyConsistentQuotaUsage object.
func NewEventuallyConsistentQuotaUsage(
	config Config, loggerSuffix string) *EventuallyConsistentQuotaUsage {
	return &EventuallyConsistentQuotaUsage{
		config: config,
		log:    config.MakeLogger(ECQUID + "-" + loggerSuffix),
	}
}

func (q *EventuallyConsistentQuotaUsage) getAndCache(
	ctx context.Context) (usage cachedQuotaUsage, err error) {
	defer func() {
		q.log.CDebugf(ctx, "getAndCache: error=%v", err)
	}()
	quotaInfo, err := q.config.BlockServer().GetUserQuotaInfo(ctx)
	if err != nil {
		return cachedQuotaUsage{}, err
	}

	usage.limitBytes = quotaInfo.Limit
	if quotaInfo.Total != nil {
		usage.usageBytes = quotaInfo.Total.Bytes[kbfsblock.UsageWrite]
	} else {
		usage.usageBytes = 0
	}
	usage.timestamp = q.config.Clock().Now()

	q.mu.Lock()
	defer q.mu.Unlock()
	q.cached = usage

	return usage, nil
}

// Get returns KBFS bytes used and limit for user. To help avoid having too
// frequent calls into bserver, caller can provide a positive tolerance, to
// accept stale LimitBytes and UsageBytes data. If tolerance is 0 or negative,
// this always makes a blocking RPC to bserver and return latest quota usage.
//
// 1) If the age of cached data is less than half of tolerance, the cached
// stale data is returned immediately.
// 2) If the age of cached data is more than half of tolerance, but not more
// than tolerance, a background RPC is spawned to refresh cached data, and the
// stale data is returned immediately.
// 3) If the age of cached data is more than tolerance, a blocking RPC is
// issued and the function only returns after RPC finishes, with the newest
// data from RPC. The RPC causes cached data to be refreshed as well.
func (q *EventuallyConsistentQuotaUsage) Get(ctx context.Context,
	tolerance time.Duration) (usageBytes, limitBytes int64, err error) {
	c := func() cachedQuotaUsage {
		q.mu.RLock()
		defer q.mu.RUnlock()
		return q.cached
	}()
	past := q.config.Clock().Now().Sub(c.timestamp)
	switch {
	case past > tolerance:
		q.log.CDebugf(ctx, "Blocking on getAndCache. Cached data is %s old.", past)
		// TODO: optimize this to make sure there's only one outstanding RPC. In
		// other words, wait for it to finish if one is already in progress.
		c, err = q.getAndCache(ctx)
		if err != nil {
			return -1, -1, err
		}
	case past > tolerance/2:
		if atomic.CompareAndSwapInt32(&q.backgroundInProcess, 0, 1) {
			id, err := MakeRandomRequestID()
			if err != nil {
				q.log.Warning("Couldn't generate a random request ID: %v", err)
			}
			q.log.CDebugf(ctx, "Cached data is %s old. Spawning getAndCache in "+
				"background with tag:%s=%v.", past, ECQUID, id)
			go func() {
				// Make a new context so that it doesn't get canceled when returned.
				logTags := make(logger.CtxLogTags)
				logTags[ECQUCtxTagKey{}] = ECQUID
				bgCtx := logger.NewContextWithLogTags(context.Background(), logTags)
				bgCtx = context.WithValue(bgCtx, ECQUCtxTagKey{}, id)
				// Make sure a timeout is on the context, in case the RPC blocks
				// forever somehow, where we'd end up with never resetting
				// backgroundInProcess flag again.
				bgCtx, cancel := context.WithTimeout(bgCtx, 10*time.Second)
				defer cancel()
				// The error is igonred here without logging since getAndCache already
				// logs it.
				_, _ = q.getAndCache(bgCtx)
				atomic.StoreInt32(&q.backgroundInProcess, 0)
			}()
		} else {
			q.log.CDebugf(ctx,
				"Cached data is %s old, but background getAndCache is already running.", past)
		}
	default:
		q.log.CDebugf(ctx, "Returning cached data from %s ago.", past)
	}
	return c.usageBytes, c.limitBytes, nil
}
