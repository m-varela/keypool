package clock

import (
	"context"
	"testing"
	"time"
)

func TestFakeClockAdvanceAndWake(t *testing.T) {
	f := NewFake(time.Unix(100, 0))
	if err := f.Wait(context.Background(), time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if f.Elapsed() != time.Minute {
		t.Fatalf("elapsed=%s", f.Elapsed())
	}
	wake := make(chan struct{})
	close(wake)
	if err := f.Wait(context.Background(), time.Hour, wake); err != nil {
		t.Fatal(err)
	}
	if f.Elapsed() != time.Minute {
		t.Fatal("wake advanced fake clock")
	}
}
func TestFakeClockCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewFake(time.Now()).Wait(ctx, time.Second, nil); err == nil {
		t.Fatal("expected context error")
	}
}
