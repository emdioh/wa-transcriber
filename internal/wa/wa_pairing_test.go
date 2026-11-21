package wa

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// A channel created for one connect attempt must survive that attempt
// failing. whatsmeow removes its QR event handler only when the channel
// closes, so a second GetQRChannel after a failed connect leaves the first
// handler behind with a cancelled context; it then disconnects the live
// socket as soon as the next QR code arrives, and the phone reports
// "cannot pair" on a code that is already dead.
func TestQRChanCacheReusesChannelAcrossFailedAttempts(t *testing.T) {
	var q qrChanCache
	var created int32
	mk := func(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
		atomic.AddInt32(&created, 1)
		return make(chan whatsmeow.QRChannelItem), nil
	}
	ctx := context.Background()

	first, err := q.get(ctx, mk)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	// Three failed connect attempts in a row: Run retries every 5 s.
	for i := 0; i < 3; i++ {
		got, err := q.get(ctx, mk)
		if err != nil {
			t.Fatalf("get %d: %v", i+2, err)
		}
		if got != first {
			t.Fatalf("get %d returned a new channel; the old handler would leak", i+2)
		}
	}
	if n := atomic.LoadInt32(&created); n != 1 {
		t.Fatalf("GetQRChannel called %d times, want 1", n)
	}

	// Once the channel is used up, the next attempt needs a new one.
	q.drop()
	second, err := q.get(ctx, mk)
	if err != nil {
		t.Fatalf("get after drop: %v", err)
	}
	if second == first {
		t.Fatal("drop did not release the channel")
	}
	if n := atomic.LoadInt32(&created); n != 2 {
		t.Fatalf("GetQRChannel called %d times after drop, want 2", n)
	}
}

func TestQRChanCacheCancelsOnDrop(t *testing.T) {
	var q qrChanCache
	ctxDone := make(chan struct{})
	mk := func(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
		go func() {
			<-ctx.Done()
			close(ctxDone)
		}()
		return make(chan whatsmeow.QRChannelItem), nil
	}
	if _, err := q.get(context.Background(), mk); err != nil {
		t.Fatalf("get: %v", err)
	}
	q.drop()
	select {
	case <-ctxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("dropping the channel left its context running")
	}
}

func TestQRChanCacheDoesNotCacheErrors(t *testing.T) {
	var q qrChanCache
	var calls int32
	mk := func(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("already connected")
	}
	for i := 0; i < 2; i++ {
		if _, err := q.get(context.Background(), mk); err == nil {
			t.Fatal("want the maker's error")
		}
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("maker called %d times, want 2", n)
	}
}
