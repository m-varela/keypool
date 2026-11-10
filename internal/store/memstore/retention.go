package memstore

import "time"

const (
	usageCapacity  = 50_000
	eventCapacity  = 10_000
	healthCapacity = 1_000

	usageRetention  = 7 * 24 * time.Hour
	eventRetention  = 30 * 24 * time.Hour
	healthRetention = 7 * 24 * time.Hour
)
