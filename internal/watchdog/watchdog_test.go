package watchdog

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDev struct {
	pets   atomic.Int64
	closes atomic.Int64
	err    error
}

func (f *fakeDev) Keepalive() error {
	f.pets.Add(1)
	return f.err
}

func (f *fakeDev) Close() error {
	f.closes.Add(1)
	return nil
}

func TestKeeperPetsAndDisarms(t *testing.T) {
	dev := &fakeDev{}
	k := &Keeper{Dev: dev, Interval: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for dev.pets.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if dev.pets.Load() < 3 {
		t.Fatalf("pets = %d", dev.pets.Load())
	}
	if dev.closes.Load() != 1 {
		t.Fatalf("closes = %d, want 1 (disarm on shutdown)", dev.closes.Load())
	}
}

func TestKeeperReportsErrors(t *testing.T) {
	var got atomic.Int64
	dev := &fakeDev{err: errors.New("boom")}
	k := &Keeper{Dev: dev, Interval: time.Millisecond, OnError: func(error) { got.Add(1) }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	k.Run(ctx)
	if got.Load() == 0 {
		t.Fatal("errors were not reported")
	}
}

func TestOpenMissingDevice(t *testing.T) {
	if _, err := Open("/nonexistent/watchdog", nil); err == nil {
		t.Fatal("expected an error for a missing device")
	}
	if _, err := Open("", nil); err == nil {
		t.Fatal("expected an error for an empty path")
	}
}

func TestKeeperDefaultsInterval(t *testing.T) {
	dev := &fakeDev{}
	k := &Keeper{Dev: dev, Interval: 0}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()
	cancel()
	<-done
	if dev.closes.Load() != 1 {
		t.Fatalf("closes = %d", dev.closes.Load())
	}
}
