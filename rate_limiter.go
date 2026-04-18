package main

import (
	"sync"
	"time"
)

type RateLimiter interface {
	IsAllowed(userID string) bool
}

type DefaultRateLimiter struct {
	requests  sync.Map // map[userID][]time.Time
	rateLimit int
	rateTime  time.Duration
}

func NewDefaultRateLimiter(rateLimit int, rateTime time.Duration) *DefaultRateLimiter {
	return &DefaultRateLimiter{
		rateLimit: rateLimit,
		rateTime:  rateTime,
	}
}

func (r *DefaultRateLimiter) IsAllowed(userID string) bool {
	now := time.Now()

	// Get existing requests
	value, ok := r.requests.Load(userID)
	var requests []time.Time
	if ok {
		requests = value.([]time.Time)
	}

	// Remove old requests
	var recentRequests []time.Time
	for _, t := range requests {
		if now.Sub(t) < r.rateTime {
			recentRequests = append(recentRequests, t)
		}
	}

	if len(recentRequests) >= r.rateLimit {
		return false
	}

	recentRequests = append(recentRequests, now)
	r.requests.Store(userID, recentRequests)

	// KISS cleanup: remove empty entries periodically
	if len(recentRequests) == 1 {
		r.cleanupOldUsers(now)
	}

	return true
}

// cleanupOldUsers removes users with no recent activity (called occasionally)
func (r *DefaultRateLimiter) cleanupOldUsers(now time.Time) {
	r.requests.Range(func(key, value interface{}) bool {
		requests := value.([]time.Time)
		if len(requests) == 0 {
			r.requests.Delete(key)
			return true
		}
		// Check if all requests are old
		allOld := true
		for _, t := range requests {
			if now.Sub(t) < r.rateTime {
				allOld = false
				break
			}
		}
		if allOld {
			r.requests.Delete(key)
		}
		return true
	})
}
