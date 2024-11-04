package logging

import (
	"context"
	"testing"

	"github.com/prysmaticlabs/prysm/v5/consensus-types/primitives"
	"github.com/prysmaticlabs/prysm/v5/time/slots"
)

// TestNewAttestationLogger verifies that a new instance of AttestationLogger is created correctly.
func TestNewAttestationLogger(t *testing.T) {
	logger := NewAttestationLogger()

	if logger == nil {
		t.Errorf("expected a new instance of AttestationLogger, got nil")
	}

	if len(logger.failureReasons) != 0 {
		t.Errorf("expected failureReasons map to be empty, got %d", len(logger.failureReasons))
	}
}

// TestSuccess verifies that the Success function correctly increments the successfulCount.
func TestSuccess(t *testing.T) {
	logger := NewAttestationLogger()

	slot := primitives.Slot(6249440)
	logger.Success(context.Background(), slot)

	if logger.successfulCount != 1 {
		t.Errorf("expected successfulCount to be 1, got %d", logger.successfulCount)
	}
}

// TestFailure verifies that the Failure function correctly increments the failedCount and updates the failureReasons map.
func TestFailure(t *testing.T) {
	logger := NewAttestationLogger()

	reason := "test failure"
	slot := primitives.Slot(6249440)
	logger.Failure(context.Background(), reason, slot)

	if logger.failedCount != 1 {
		t.Errorf("expected failedCount to be 1, got %d", logger.failedCount)
	}

	if count, exists := logger.failureReasons[reason]; !exists || count != 1 {
		t.Errorf("expected failureReasons[%s] to be 1, got %d", reason, count)
	}
}

// TestSummary verifies that the Summary function correctly logs the summary and resets the counters and map.
func TestSummary(t *testing.T) {
	logger := NewAttestationLogger()

	ctx := context.Background()
	slot := primitives.Slot(6249440)
	epoch := slots.ToEpoch(slot / 32)

	logger.Success(ctx, slot)
	logger.Failure(ctx, "test failure 1", slot)
	logger.Failure(ctx, "test failure 2", slot)

	logger.Summary(ctx, epoch)

	if logger.currentEpoch != epoch {
		t.Errorf("expected epoch to be %d got %v", logger.currentEpoch, epoch)
	}

}

// TestResetCounters verifies that the resetCounters function correctly resets the counters and map.
func TestResetCounters(t *testing.T) {
	logger := NewAttestationLogger()

	ctx := context.Background()
	slot := primitives.Slot(6249440)

	logger.Success(ctx, slot)
	logger.Failure(ctx, "test failure 1", slot)
	logger.Failure(ctx, "test failure 2", slot)

	logger.resetCounters(ctx)

	if logger.successfulCount != 0 {
		t.Errorf("expected successfulCount to be reset to 0, got %d", logger.successfulCount)
	}

	if logger.failedCount != 0 {
		t.Errorf("expected failedCount to be reset to 0, got %d", logger.failedCount)
	}

	if len(logger.failureReasons) != 0 {
		t.Errorf("expected failureReasons to be reset to empty, got %d", len(logger.failureReasons))
	}
}
