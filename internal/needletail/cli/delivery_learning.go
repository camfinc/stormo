package cli

import (
	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/sched"
)

// deliveryLearning records only scheduler-verified report outcomes. Payloads and
// delivery targets never enter this path, and unknown scope stays private.
func deliveryLearning(agent *loop.Agent, logf func(string)) func(sched.DeliveryEvent) {
	return func(event sched.DeliveryEvent) {
		if agent == nil || agent.Learning == nil {
			return
		}
		root := ""
		if event.Session != "" {
			if agent.Store == nil {
				return
			}
			if agent.Store.Untrusted(event.Session) {
				return
			}
			var ok bool
			root, ok = agent.Store.Root(event.Session)
			if !ok {
				if logf != nil {
					logf("delivery evidence skipped: run session is unavailable")
				}
				return
			}
		}
		record := func() error {
			return agent.Learning.RecordDelivery(runtimelearn.DeliveryObservation{ID: event.ID, JobID: event.JobID, TaskRoot: root, At: event.At, Status: event.Status, Failure: event.Failure})
		}
		var err error
		if event.Session != "" {
			err = agent.Store.WithTrusted(event.Session, record)
		} else {
			err = record()
		}
		if err != nil && logf != nil {
			logf("delivery evidence could not be retained; delivery handling continues")
		}
	}
}
