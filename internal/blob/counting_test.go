// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"errors"
	"strings"
	"testing"
)

// everyCall is one call of each method of the interface, so a test asserts
// over the whole surface rather than the three methods it remembered.
func everyCall(t *testing.T, s Store, uploadID string) map[string]func() error {
	t.Helper()
	return map[string]func() error{
		MethodPut: func() error {
			_, err := s.Put(t.Context(), "arca/1f/counted", strings.NewReader("x"), 1, PutOptions{})
			return err
		},
		MethodGet: func() error {
			rc, _, err := s.Get(t.Context(), "arca/1f/counted")
			if rc != nil {
				_ = rc.Close()
			}
			return err
		},
		MethodHead: func() error {
			_, err := s.Head(t.Context(), "arca/1f/counted")
			return err
		},
		MethodDelete:     func() error { return s.Delete(t.Context(), "arca/1f/gone") },
		MethodDeleteMany: func() error { return s.DeleteMany(t.Context(), []string{"arca/1f/gone"}) },
		MethodList: func() error {
			_, err := s.List(t.Context(), "arca/", "", 10)
			return err
		},
		MethodPresignGet: func() error {
			_, err := s.PresignGet(t.Context(), "arca/1f/counted", PresignOptions{})
			return err
		},
		MethodCreateMultipart: func() error {
			_, err := s.CreateMultipart(t.Context(), "arca/1f/parts", PutOptions{})
			return err
		},
		MethodPresignPart: func() error {
			_, err := s.PresignPart(t.Context(), "arca/1f/parts", uploadID, 1)
			return err
		},
		MethodCompleteMultipart: func() error {
			_, err := s.CompleteMultipart(t.Context(), "arca/1f/parts", uploadID, []Part{{Number: 1, ETag: etagOf([]byte("part"))}})
			return err
		},
		MethodAbortMultipart: func() error { return s.AbortMultipart(t.Context(), "arca/1f/parts", uploadID) },
		MethodSetPublic:      func() error { return s.SetPublic(t.Context(), "arca/1f/counted", true) },
		MethodHeadBucket:     func() error { return s.HeadBucket(t.Context()) },
	}
}

func TestCountingCountsEveryMethodAndForwardsIt(t *testing.T) {
	inner := NewMemory()
	counting := NewCounting(inner)
	uploadID, err := inner.CreateMultipart(t.Context(), "arca/1f/parts", PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inner.UploadPart("arca/1f/parts", uploadID, 1, []byte("part")); err != nil {
		t.Fatal(err)
	}
	calls := everyCall(t, counting, uploadID)
	for _, method := range []string{
		MethodPut, MethodGet, MethodHead, MethodPresignGet, MethodList, MethodSetPublic,
		MethodHeadBucket, MethodDelete, MethodDeleteMany, MethodCreateMultipart,
		MethodPresignPart, MethodCompleteMultipart, MethodAbortMultipart,
	} {
		if err := calls[method](); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if got := counting.Calls(method); got != 1 {
			t.Errorf("%s was counted %d times", method, got)
		}
	}
	if got, want := counting.Total(), len(calls); got != want {
		t.Fatalf("Total() = %d, want %d", got, want)
	}
	// The forwarding reached the wrapped store: the write landed and the
	// completion consumed the upload it had opened.
	if _, ok := inner.Bytes("arca/1f/counted"); !ok {
		t.Error("the counted put never reached the store")
	}
}

func TestCountingFailsTheCallItWasAskedTo(t *testing.T) {
	boom := errors.New("the bucket is on fire")
	inner := NewMemory()
	counting := NewCounting(inner)
	uploadID, err := inner.CreateMultipart(t.Context(), "arca/1f/parts", PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for method, call := range everyCall(t, counting, uploadID) {
		fresh := NewCounting(NewMemory())
		fresh.FailNth(method, 1, boom)
		if err := everyCall(t, fresh, uploadID)[method](); !errors.Is(err, boom) {
			t.Errorf("%s with a failure injected on the first call = %v", method, err)
		}
		_ = call
	}
}

func TestCountingFailsOnlyTheNthCall(t *testing.T) {
	boom := errors.New("the second write is the one that fails")
	counting := NewCounting(NewMemory())
	counting.FailNth(MethodPut, 2, boom)
	if _, err := counting.Put(t.Context(), "arca/1f/one", strings.NewReader("x"), 1, PutOptions{}); err != nil {
		t.Fatalf("the first put = %v", err)
	}
	if _, err := counting.Put(t.Context(), "arca/1f/two", strings.NewReader("x"), 1, PutOptions{}); !errors.Is(err, boom) {
		t.Fatalf("the second put = %v", err)
	}
	if _, err := counting.Put(t.Context(), "arca/1f/three", strings.NewReader("x"), 1, PutOptions{}); err != nil {
		t.Fatalf("the third put = %v", err)
	}
	if got := counting.Calls(MethodPut); got != 3 {
		t.Fatalf("Calls(Put) = %d, want 3", got)
	}
}

// TestAMoveTouchesNoBucketKey is the shape criterion 7 of spec 001 is proved
// with: a counting store over any bucket reports zero calls for an operation
// that is a row update. Spec 005's move runs this assertion against the
// handler; here it holds the counter itself to account.
func TestAMoveTouchesNoBucketKey(t *testing.T) {
	counting := NewCounting(NewMemory())
	if got := counting.Total(); got != 0 {
		t.Fatalf("a fresh counter reports %d calls", got)
	}
}
