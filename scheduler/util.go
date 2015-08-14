package scheduler

import (
	"fmt"

	"github.com/hashicorp/nomad/nomad/structs"
)

// allocTuple is a tuple of the allocation name and potential alloc ID
type allocTuple struct {
	Name      string
	TaskGroup *structs.TaskGroup
	Alloc     *structs.Allocation
}

// materializeTaskGroups is used to materialize all the task groups
// a job requires. This is used to do the count expansion.
func materializeTaskGroups(job *structs.Job) map[string]*structs.TaskGroup {
	out := make(map[string]*structs.TaskGroup)
	for _, tg := range job.TaskGroups {
		for i := 0; i < tg.Count; i++ {
			name := fmt.Sprintf("%s.%s[%d]", job.Name, tg.Name, i)
			out[name] = tg
		}
	}
	return out
}

// diffAllocs is used to do a set difference between the target allocations
// and the existing allocations. This returns 5 sets of results, the list of
// named task groups that need to be placed (no existing allocation), the
// allocations that need to be updated (job definition is newer), allocs that
// need to be migrated (node is draining), the allocs that need to be evicted
// (no longer required), and those that should be ignored.
func diffAllocs(job *structs.Job,
	taintedNodes map[string]bool,
	required map[string]*structs.TaskGroup,
	allocs []*structs.Allocation) (place, update, migrate, evict, ignore []allocTuple) {

	// Scan the existing updates
	existing := make(map[string]struct{})
	for _, exist := range allocs {
		// Index the existing node
		name := exist.Name
		existing[name] = struct{}{}

		// Check for the definition in the required set
		tg, ok := required[name]

		// If not required, we evict
		if !ok {
			evict = append(evict, allocTuple{
				Name:      name,
				TaskGroup: tg,
				Alloc:     exist,
			})
			continue
		}

		// If we are on a tainted node, we must migrate
		if taintedNodes[exist.NodeID] {
			migrate = append(migrate, allocTuple{
				Name:      name,
				TaskGroup: tg,
				Alloc:     exist,
			})
			continue
		}

		// If the definition is updated we need to update
		// XXX: This is an extremely conservative approach. We can check
		// if the job definition has changed in a way that affects
		// this allocation and potentially ignore it.
		if job.ModifyIndex != exist.Job.ModifyIndex {
			update = append(update, allocTuple{
				Name:      name,
				TaskGroup: tg,
				Alloc:     exist,
			})
			continue
		}

		// Everything is up-to-date
		ignore = append(ignore, allocTuple{
			Name:      name,
			TaskGroup: tg,
			Alloc:     exist,
		})
	}

	// Scan the required groups
	for name, tg := range required {
		// Check for an existing allocation
		_, ok := existing[name]

		// Require a placement if no existing allocation. If there
		// is an existing allocation, we would have checked for a potential
		// update or ignore above.
		if !ok {
			place = append(place, allocTuple{
				Name:      name,
				TaskGroup: tg,
			})
		}
	}
	return
}

// readyNodesInDCs returns all the ready nodes in the given datacenters
func readyNodesInDCs(state State, dcs []string) ([]*structs.Node, error) {
	var out []*structs.Node
	for _, dc := range dcs {
		iter, err := state.NodesByDatacenterStatus(dc, structs.NodeStatusReady)
		if err != nil {
			return nil, err
		}
		for {
			raw := iter.Next()
			if raw == nil {
				break
			}
			out = append(out, raw.(*structs.Node))
		}
	}
	return out, nil
}

// retryMax is used to retry a callback until it returns success or
// a maximum number of attempts is reached
func retryMax(max int, cb func() (bool, error)) error {
	attempts := 0
	for attempts < max {
		done, err := cb()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		attempts += 1
	}
	return fmt.Errorf("maximum attempts reached (%d)", max)
}

// taintedNodes is used to scan the allocations and then check if the
// underlying nodes are tainted, and should force a migration of the allocation.
func taintedNodes(state State, allocs []*structs.Allocation) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, alloc := range allocs {
		if _, ok := out[alloc.NodeID]; ok {
			continue
		}

		node, err := state.GetNodeByID(alloc.NodeID)
		if err != nil {
			return nil, err
		}

		// If the node does not exist, we should migrate
		if node == nil {
			out[alloc.NodeID] = true
			continue
		}

		out[alloc.NodeID] = structs.ShouldDrainNode(node.Status)
	}
	return out, nil
}