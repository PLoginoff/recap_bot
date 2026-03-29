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
	return true
}
