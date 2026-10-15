package intake

import "time"

// SetTestHooks replaces the retry timer and observes each evaluation.
func (i *Intake) SetTestHooks(after func(time.Duration) <-chan time.Time, synced func()) {
	i.after, i.synced = after, synced
}
