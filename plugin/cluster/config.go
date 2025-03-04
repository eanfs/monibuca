package plugin_claster

import "time"

// ResourceConfig defines resource limits and reservations for a node
type ResourceConfig struct {
	MaxStreams           int
	MaxBandwidthMbps     int
	ReserveCPUPercent    float64
	ReserveMemoryPercent float64
}

// MonitoringConfig defines monitoring settings
type MonitoringConfig struct {
	MetricsInterval time.Duration `default:"1s"`
	AlertThresholds struct {
		CPUPercent    float64 `default:"80"`
		MemoryPercent float64 `default:"80"`
		BandwidthMbps int     `default:"1000"`
	}
}

// LoadBalancingConfig defines load balancing settings
type LoadBalancingConfig struct {
	Strategy      string
	CheckInterval time.Duration `default:"5s"`
	Weights       struct {
		CPU     float64
		Memory  float64
		Network float64
		Streams float64
	}
}

// SyncConfig defines synchronization settings
type SyncConfig struct {
	GossipInterval          time.Duration `default:"1s"`
	FullSyncInterval        time.Duration `default:"30s"`
	IncrementalSyncInterval time.Duration `default:"5s"`
	MaxStreamsPerRequest    int           `default:"100"`
	SyncRetryInterval       time.Duration `default:"5s"`
	MaxRetries              int           `default:"3"`
}
