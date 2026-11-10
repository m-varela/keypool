package pgstore

import (
	"context"
	"testing"
	"time"
)

func TestAcquirePoolAdvisoryLockRejectsNilPool(t *testing.T) {
	if _, err := AcquirePoolAdvisoryLock(context.Background(), nil, 42); err == nil {
		t.Fatal("expected nil pool error")
	}
}

func TestRetentionDefaults(t *testing.T) {
	r := (Retention{}).normalized()
	if r.UsageEvents != 30*24*time.Hour || r.HealthChecks != 7*24*time.Hour || r.Events != 90*24*time.Hour {
		t.Fatalf("unexpected retention defaults: %+v", r)
	}
	custom := Retention{UsageEvents: time.Hour, HealthChecks: 2 * time.Hour, Events: 3 * time.Hour}.normalized()
	if custom.UsageEvents != time.Hour || custom.HealthChecks != 2*time.Hour || custom.Events != 3*time.Hour {
		t.Fatalf("custom retention overwritten: %+v", custom)
	}
}
