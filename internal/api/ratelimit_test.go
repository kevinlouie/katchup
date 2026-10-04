package api

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < loginMaxFailures-1; i++ {
		l.fail("1.2.3.4")
	}
	if _, ok := l.allow("1.2.3.4"); !ok {
		t.Fatal("locked out before reaching the failure limit")
	}
	l.fail("1.2.3.4")
	wait, ok := l.allow("1.2.3.4")
	if ok || wait != loginLockout {
		t.Fatalf("expected lockout of %v, got ok=%v wait=%v", loginLockout, ok, wait)
	}
	if _, ok := l.allow("5.6.7.8"); !ok {
		t.Fatal("lockout leaked to another address")
	}

	now = now.Add(loginLockout + time.Second)
	if _, ok := l.allow("1.2.3.4"); !ok {
		t.Fatal("still locked out after the lockout elapsed")
	}

	// Failures spread wider than the window never lock out.
	for i := 0; i < 2*loginMaxFailures; i++ {
		l.fail("9.9.9.9")
		now = now.Add(loginWindow/time.Duration(loginMaxFailures-1) + time.Second)
	}
	if _, ok := l.allow("9.9.9.9"); !ok {
		t.Fatal("slow failures locked the address out")
	}

	// A success resets the count.
	for i := 0; i < loginMaxFailures-1; i++ {
		l.fail("2.2.2.2")
	}
	l.success("2.2.2.2")
	l.fail("2.2.2.2")
	if _, ok := l.allow("2.2.2.2"); !ok {
		t.Fatal("success did not reset the failure count")
	}
}

func TestUIAuth_LoginLockout(t *testing.T) {
	a := newTestUIAuth(t, "correct-horse")
	h := protectedMux(a)

	for i := 0; i < loginMaxFailures; i++ {
		postForm(h, "/login", url.Values{"key": {"wrong"}})
	}

	// Locked out: even the right key is refused, without being checked.
	w := postForm(h, "/login", url.Values{"key": {"correct-horse"}})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 while locked out, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}

	a.limiter.now = func() time.Time { return time.Now().Add(loginLockout + time.Second) }
	w = postForm(h, "/login", url.Values{"key": {"correct-horse"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected login to work after the lockout, got %d", w.Code)
	}
}
