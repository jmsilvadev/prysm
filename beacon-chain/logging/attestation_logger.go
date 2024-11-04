package logging

import (
	"context"
	"fmt"
	"sync"

	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/v5/consensus-types/primitives"
	prysmTrace "github.com/prysmaticlabs/prysm/v5/monitoring/tracing/trace"
	"github.com/prysmaticlabs/prysm/v5/time/slots"
)

type AttestationLogger struct {
	mu              sync.Mutex
	successfulCount int
	failedCount     int
	currentEpoch    primitives.Epoch
	failureReasons  map[string]int
}

// AttestationLoggerInstance is a global instance of AttestationLogger
var AttestationLoggerInstance = NewAttestationLogger()

// NewAttestationLogger creates a new instance of AttestationLogger
func NewAttestationLogger() *AttestationLogger {
	_, span := prysmTrace.StartSpan(context.Background(), "processPendingBlocks")
	defer span.End()

	return &AttestationLogger{
		failureReasons: make(map[string]int),
	}
}

// Success increments the count of successful attestations
func (al *AttestationLogger) Success(ctx context.Context, slot primitives.Slot) {
	_, span := prysmTrace.StartSpan(ctx, "logSuccess")
	defer span.End()

	al.mu.Lock()
	defer al.mu.Unlock()

	epoch := slots.ToEpoch(slot)
	if al.currentEpoch == epoch || al.currentEpoch == 0 {
		helpers.AttSuccessfullCount.Inc() // promethes metrics
		al.successfulCount++
	}

	log.Info(fmt.Sprintf("Successful verified attestation for epoch %v", epoch))
}

// Failure increments the count of failed attestations and logs the reason
func (al *AttestationLogger) Failure(ctx context.Context, reason string, slot primitives.Slot) {
	_, span := prysmTrace.StartSpan(ctx, fmt.Sprintf("logFailure: %s", reason))
	defer span.End()

	al.mu.Lock()
	defer al.mu.Unlock()

	epoch := slots.ToEpoch(slot)
	if al.currentEpoch == epoch || al.currentEpoch == 0 {
		helpers.AttFailedCount.Inc() // promethes metrics
		al.failedCount++
		al.failureReasons[reason]++
	}

	log.Error(fmt.Sprintf("Failed attestation for epoch %v", epoch))
}

// Summary logs a summary of successful and failed attestations
func (al *AttestationLogger) Summary(ctx context.Context, epoch primitives.Epoch) {
	_, span := prysmTrace.StartSpan(ctx, "outputSummary")
	defer span.End()

	if al.currentEpoch != epoch {

		al.mu.Lock()
		defer al.mu.Unlock()

		log.Info("##################################")
		log.Info(fmt.Sprintf("Attestation stats for epoch: %d", al.currentEpoch))
		log.Info(fmt.Sprintf("Successful verified attestations: %d", al.successfulCount))
		log.Info(fmt.Sprintf("Failed attestations: %d", al.failedCount))
		log.Info("##################################")

		for reason, count := range al.failureReasons {
			log.Info(fmt.Sprintf("Failure reason: %s, count: %d", reason, count))
		}

		al.currentEpoch = epoch
		al.resetCounters(ctx)
	}
}

// ResetCounters resets the counters and map
func (al *AttestationLogger) resetCounters(ctx context.Context) {
	_, span := prysmTrace.StartSpan(ctx, "resetCounters")
	defer span.End()

	// reset metrics
	helpers.AttSuccessfullCount.Set(0)
	helpers.AttSuccessfullCount.Set(0)

	// Reset counters for the next epoch
	al.successfulCount = 0
	al.failedCount = 0
	al.failureReasons = make(map[string]int)

	log.Info("Reseting attestation counters")
}

// NOTE: If needed we can add this verification to avoid OOM
// checkAndResetIfNeeded checks the size of the failureReasons map and resets it if it exceeds the maxFailures limit
/*func (al *AttestationLogger) checkAndResetIfNeeded() {
	if len(al.failureReasons) > al.maxFailures {
		log.Printf("Failure reasons map size exceeded %d entries, resetting map", al.maxFailures)
		al.Summary()
	}
}*/
