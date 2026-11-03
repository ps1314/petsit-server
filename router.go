package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrAIUnavailable means every model that could serve the request is down or tripped; callers
// degrade to their non-AI path.
var ErrAIUnavailable = errors.New("AI 暂时不可用")

// Breaker stops calling a model after Failures consecutive errors, for Cooldown. After the
// cooldown one request probes the model: success closes the breaker, failure opens it again.
// A sitter waiting on a report then skips a dead model at once instead of waiting out its timeout.
type Breaker struct {
	Failures int
	Cooldown time.Duration
	now      func() time.Time

	mu        sync.Mutex
	fails     int
	openUntil time.Time
	probing   bool
}

type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

func (b *Breaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *Breaker) stateLocked(now time.Time) BreakerState {
	switch {
	case b.openUntil.IsZero():
		return BreakerClosed
	case now.Before(b.openUntil):
		return BreakerOpen
	default:
		return BreakerHalfOpen
	}
}

func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked(b.clock())
}

// Allow reports whether a call may go out now and whether that call is the half-open probe;
// only one probe is in flight at a time.
func (b *Breaker) Allow() (ok, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.stateLocked(b.clock()) {
	case BreakerOpen:
		return false, false
	case BreakerHalfOpen:
		if b.probing {
			return false, false
		}
		b.probing = true
		return true, true
	}
	return true, false
}

func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails, b.openUntil, b.probing = 0, time.Time{}, false
}

// Failure records an error and reports whether it opened the breaker. A failed probe reopens it.
func (b *Breaker) Failure(probe bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
	}
	b.fails++
	if probe || b.fails >= max(b.Failures, 1) {
		b.fails = 0
		b.openUntil = b.clock().Add(b.Cooldown)
		return true
	}
	return false
}

// Release gives back a probe whose call ended without telling anything about the model (the
// caller gave up), so the next request can probe instead.
func (b *Breaker) Release(probe bool) {
	if !probe {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

// Fallback tries each model in order, giving each its own deadline and a circuit breaker, so an
// outage of the primary model still yields an answer within the sitter's wait.
type Fallback struct {
	Models    []LLM
	PerTry    time.Duration
	RetryWait time.Duration // pause before the one retry of a busy (429/503) model
	// Breaker settings applied to every model; Failures 0 disables the breakers.
	BreakerFailures int
	BreakerCooldown time.Duration
	OnError         func(model string, err error)
	OnTrip          func(model string)

	once     sync.Once
	breakers []*Breaker // one per slot in Models, so two slots with the same model stay apart
	now      func() time.Time
}

func (f *Fallback) breaker(i int) *Breaker {
	f.once.Do(func() {
		for range f.Models {
			f.breakers = append(f.breakers, &Breaker{Failures: f.BreakerFailures, Cooldown: f.BreakerCooldown, now: f.now})
		}
	})
	if f.BreakerFailures <= 0 {
		return nil
	}
	return f.breakers[i]
}

// HasVision reports whether any configured model can read photos.
func (f *Fallback) HasVision() bool {
	for _, m := range f.Models {
		if m.Vision() {
			return true
		}
	}
	return false
}

// Status lists each model with its breaker state, for logs and the status endpoint.
func (f *Fallback) Status() []map[string]any {
	out := []map[string]any{}
	for i, m := range f.Models {
		state := BreakerClosed
		if b := f.breaker(i); b != nil {
			state = b.State()
		}
		out = append(out, map[string]any{"model": m.Name(), "vision": m.Vision(), "state": state})
	}
	return out
}

// Do sends req down the chain until a model answers and accept takes the answer. A reply accept
// rejects (not JSON, wrong shape) counts as that model failing. Models without vision are skipped
// for requests with images. ErrNoModel means nothing is configured; ErrAIUnavailable wraps the
// case where every candidate failed or was tripped.
func (f *Fallback) Do(ctx context.Context, req ChatRequest, accept func(raw string) error) (string, string, error) {
	var candidates []int
	for i, m := range f.Models {
		if len(req.Images) == 0 || m.Vision() {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return "", "", ErrNoModel
	}
	var errs []error
	for _, i := range candidates {
		m, b := f.Models[i], f.breaker(i)
		probe := false
		if b != nil {
			var ok bool
			if ok, probe = b.Allow(); !ok {
				errs = append(errs, fmt.Errorf("%s: circuit open", m.Name()))
				continue
			}
		}
		raw, err := f.try(ctx, m, req, accept)
		if err == nil {
			if b != nil {
				b.Success()
			}
			return m.Name(), raw, nil
		}
		if ctx.Err() != nil {
			// The caller gave up; that says nothing about the model.
			if b != nil {
				b.Release(probe)
			}
			errs = append(errs, err)
			break
		}
		if f.OnError != nil {
			f.OnError(m.Name(), err)
		}
		if b != nil && b.Failure(probe) && f.OnTrip != nil {
			f.OnTrip(m.Name())
		}
		errs = append(errs, err)
	}
	return "", "", fmt.Errorf("%w: %w", ErrAIUnavailable, errors.Join(errs...))
}

func (f *Fallback) try(ctx context.Context, m LLM, req ChatRequest, accept func(string) error) (string, error) {
	call := func() (string, error) {
		tctx, cancel := context.WithTimeout(ctx, f.PerTry)
		defer cancel()
		raw, err := m.Chat(tctx, req)
		if err != nil {
			return raw, err
		}
		if err := accept(raw); err != nil {
			return raw, fmt.Errorf("%s: %w", m.Name(), err)
		}
		return raw, nil
	}
	raw, err := call()
	// "Service is too busy" and rate limits are usually gone a moment later.
	if err != nil && busy(err) && ctx.Err() == nil {
		select {
		case <-time.After(f.RetryWait):
		case <-ctx.Done():
			return raw, err
		}
		raw, err = call()
	}
	return raw, err
}

func busy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "HTTP 503") || strings.Contains(msg, "HTTP 429")
}
