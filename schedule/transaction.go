// Agent-scoped serialization for Schedule reads and durable mutations.
package schedule

import (
	"sync"

	"dshgo/agent"
)

// transactionTails is the per-agent serialization keyed by the exact agent
// pointer (the official WeakMap tail chain).
var transactionTails = struct {
	sync.Mutex
	byAgent map[*agent.Agent]*agentTransaction
}{byAgent: map[*agent.Agent]*agentTransaction{}}

type agentTransaction struct {
	// done closes when the transaction's operation settles (success or
	// failure); it is the next transaction's predecessor.
	done chan struct{}
}

// RunScheduleTransaction runs one complete Schedule transaction after its
// exact agent's prior transaction.
func RunScheduleTransaction[T any](ag *agent.Agent, operation func() (T, error)) (T, error) {
	transactionTails.Lock()
	prior := transactionTails.byAgent[ag]
	tail := &agentTransaction{done: make(chan struct{})}
	transactionTails.byAgent[ag] = tail
	transactionTails.Unlock()
	if prior != nil {
		<-prior.done
	}
	defer func() {
		close(tail.done)
		transactionTails.Lock()
		if transactionTails.byAgent[ag] == tail {
			delete(transactionTails.byAgent, ag)
		}
		transactionTails.Unlock()
	}()
	return operation()
}
