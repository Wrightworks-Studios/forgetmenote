package notestore

import (
	"context"
	"testing"
	"time"
)

// ScheduleBlockSweepsForTest makes the stores the test opens from now on run
// the scheduled block sweep after delay and then every every, soon after a
// delete, and retry a purge's failed removal of freed blocks after retry,
// until the test ends.
func ScheduleBlockSweepsForTest(t testing.TB, delay, every, soon, retry time.Duration) {
	t.Helper()
	old := [5]time.Duration{blockSweepDelay, blockSweepJitter, blockSweepEvery, blockSweepSoon, purgeRetry}
	blockSweepDelay, blockSweepJitter, blockSweepEvery, blockSweepSoon, purgeRetry = delay, 0, every, soon, retry
	t.Cleanup(func() {
		blockSweepDelay, blockSweepJitter, blockSweepEvery, blockSweepSoon, purgeRetry = old[0], old[1], old[2], old[3], old[4]
	})
}

// ScheduledSweepForTest runs one scheduled block sweep now.
func (s *Store) ScheduledSweepForTest(ctx context.Context) (SweepResult, bool, error) {
	return s.scheduledSweep(ctx)
}
