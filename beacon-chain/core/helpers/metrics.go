package helpers

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	attReceivedTooEarlyCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "attestation_too_early_total",
		Help: "Increased when an attestation is considered too early",
	})
	attReceivedTooLateCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "attestation_too_late_total",
		Help: "Increased when an attestation is considered too late",
	})
	AttSuccessfullCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "attestation_succesfull",
		Help: "Increased when an attestation is considered verified",
	})
	AttFailedCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "attestation_failed",
		Help: "Increased when an attestation failed",
	})
)
