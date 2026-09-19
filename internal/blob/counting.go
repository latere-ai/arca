// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// The method names Counting keys its counters on.
const (
	MethodPut               = "Put"
	MethodCopy              = "Copy"
	MethodGet               = "Get"
	MethodHead              = "Head"
	MethodDelete            = "Delete"
	MethodDeleteMany        = "DeleteMany"
	MethodList              = "List"
	MethodPresignGet        = "PresignGet"
	MethodCreateMultipart   = "CreateMultipart"
	MethodPresignPart       = "PresignPart"
	MethodCompleteMultipart = "CompleteMultipart"
	MethodAbortMultipart    = "AbortMultipart"
	MethodSetPublic         = "SetPublic"
	MethodHeadBucket        = "HeadBucket"
)

// Counting wraps any Store, counts the calls per method, and fails a chosen
// call of one method. It is how a test proves a negative: spec 005 asserts
// that a move leaves every counter at zero, and spec 010 injects the fault
// that leaves bytes in the bucket with no row.
type Counting struct {
	inner Store

	mu     sync.Mutex
	calls  map[string]int
	faults map[string]fault
}

// fault is one injected failure: the nth call of a method returns err.
type fault struct {
	nth int
	err error
}

// NewCounting wraps a store.
func NewCounting(inner Store) *Counting {
	return &Counting{inner: inner, calls: map[string]int{}, faults: map[string]fault{}}
}

// Calls answers how often the method was called.
func (c *Counting) Calls(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

// Total answers how often the store was called at all, so a test asserts
// that an operation reached no bucket.
func (c *Counting) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, n := range c.calls {
		total += n
	}
	return total
}

// FailNth makes the nth call of the method return err, counting from one.
// The call does not reach the wrapped store.
func (c *Counting) FailNth(method string, nth int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults[method] = fault{nth: nth, err: err}
}

// enter counts one call and answers the injected failure that belongs to it.
func (c *Counting) enter(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[method]++
	if f, ok := c.faults[method]; ok && f.nth == c.calls[method] {
		return fmt.Errorf("blob: injected failure on %s call %d: %w", method, f.nth, f.err)
	}
	return nil
}

// Put counts and forwards.
func (c *Counting) Put(ctx context.Context, key string, body io.Reader, size int64, o PutOptions) (Written, error) {
	if err := c.enter(MethodPut); err != nil {
		return Written{}, err
	}
	return c.inner.Put(ctx, key, body, size, o)
}

// Copy counts and forwards. The object move of spec 019 asserts the negative
// through it: a run that never deletes leaves MethodDelete and
// MethodDeleteMany at zero.
func (c *Counting) Copy(ctx context.Context, from, to string, o PutOptions) (Object, error) {
	if err := c.enter(MethodCopy); err != nil {
		return Object{}, err
	}
	return c.inner.Copy(ctx, from, to, o)
}

// Get counts and forwards.
func (c *Counting) Get(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	if err := c.enter(MethodGet); err != nil {
		return nil, Object{}, err
	}
	return c.inner.Get(ctx, key)
}

// Head counts and forwards.
func (c *Counting) Head(ctx context.Context, key string) (Object, error) {
	if err := c.enter(MethodHead); err != nil {
		return Object{}, err
	}
	return c.inner.Head(ctx, key)
}

// Delete counts and forwards.
func (c *Counting) Delete(ctx context.Context, key string) error {
	if err := c.enter(MethodDelete); err != nil {
		return err
	}
	return c.inner.Delete(ctx, key)
}

// DeleteMany counts and forwards.
func (c *Counting) DeleteMany(ctx context.Context, keys []string) error {
	if err := c.enter(MethodDeleteMany); err != nil {
		return err
	}
	return c.inner.DeleteMany(ctx, keys)
}

// List counts and forwards.
func (c *Counting) List(ctx context.Context, prefix, startAfter string, max int32) (Listing, error) {
	if err := c.enter(MethodList); err != nil {
		return Listing{}, err
	}
	return c.inner.List(ctx, prefix, startAfter, max)
}

// PresignGet counts and forwards.
func (c *Counting) PresignGet(ctx context.Context, key string, o PresignOptions) (string, error) {
	if err := c.enter(MethodPresignGet); err != nil {
		return "", err
	}
	return c.inner.PresignGet(ctx, key, o)
}

// CreateMultipart counts and forwards.
func (c *Counting) CreateMultipart(ctx context.Context, key string, o PutOptions) (string, error) {
	if err := c.enter(MethodCreateMultipart); err != nil {
		return "", err
	}
	return c.inner.CreateMultipart(ctx, key, o)
}

// PresignPart counts and forwards.
func (c *Counting) PresignPart(ctx context.Context, key, uploadID string, part int32) (string, error) {
	if err := c.enter(MethodPresignPart); err != nil {
		return "", err
	}
	return c.inner.PresignPart(ctx, key, uploadID, part)
}

// CompleteMultipart counts and forwards.
func (c *Counting) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Object, error) {
	if err := c.enter(MethodCompleteMultipart); err != nil {
		return Object{}, err
	}
	return c.inner.CompleteMultipart(ctx, key, uploadID, parts)
}

// AbortMultipart counts and forwards.
func (c *Counting) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if err := c.enter(MethodAbortMultipart); err != nil {
		return err
	}
	return c.inner.AbortMultipart(ctx, key, uploadID)
}

// SetPublic counts and forwards.
func (c *Counting) SetPublic(ctx context.Context, key string, public bool) error {
	if err := c.enter(MethodSetPublic); err != nil {
		return err
	}
	return c.inner.SetPublic(ctx, key, public)
}

// HeadBucket counts and forwards.
func (c *Counting) HeadBucket(ctx context.Context) error {
	if err := c.enter(MethodHeadBucket); err != nil {
		return err
	}
	return c.inner.HeadBucket(ctx)
}
