package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type bodyAdmission struct {
	mu   sync.Mutex
	cond *sync.Cond
	free int64
}

var errAdmissionTimeout = errors.New("server busy: request body budget exhausted")

func newBodyAdmission(budget int64) *bodyAdmission {
	admission := &bodyAdmission{free: budget}
	admission.cond = sync.NewCond(&admission.mu)
	return admission
}

func (b *bodyAdmission) reserve(ctx context.Context, n int64) error {
	if b == nil {
		return nil
	}
	bump := func() {
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	}
	var expired atomic.Bool
	stopOnCancel := context.AfterFunc(ctx, bump)
	defer stopOnCancel()
	timer := time.AfterFunc(bodyAdmissionTimeout, func() {
		expired.Store(true)
		bump()
	})
	defer timer.Stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	for b.free < n {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case expired.Load():
			return errAdmissionTimeout
		}
		b.cond.Wait()
	}
	b.free -= n
	return nil
}

func (b *bodyAdmission) release(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.free += n
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (p *Proxy) admitBody(r *http.Request) (func(), error) {
	charge := chargeForBody(r.ContentLength)
	if err := p.bodies.reserve(r.Context(), charge); err != nil {
		return func() {}, err
	}
	return func() { p.bodies.release(charge) }, nil
}

func chargeForBody(contentLength int64) int64 {
	if contentLength <= 0 {
		return maxBodyBytes
	}
	return min(contentLength, maxBodyBytes)
}
