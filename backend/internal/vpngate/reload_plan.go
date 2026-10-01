package vpngate

// ReloadPlan is what a pool reload renders: every slot's group holds the
// candidates, and a leased slot whose node left the VPN Gate list also holds
// that node, which no other group does.
type ReloadPlan struct {
	Candidates []Node
	Extras     map[string][]Node // slot name -> node it keeps
}

// PlanReload keeps a leased slot on its node when the node left the list:
// the node's previous definition is carried over for that slot only, so the
// account behind the lease keeps its exit until health checks move the slot.
// known holds every node the running config defines; current maps slot name
// to the node the slot uses now.
func PlanReload(known map[string]Node, next []Node, active []Slot, current map[string]string) ReloadPlan {
	listed := make(map[string]bool, len(next))
	for _, n := range next {
		listed[n.Name] = true
	}
	plan := ReloadPlan{Candidates: next, Extras: map[string][]Node{}}
	for _, s := range active {
		cur, ok := current[s.Name]
		if !ok || listed[cur] {
			continue
		}
		if n, ok := known[cur]; ok {
			plan.Extras[s.Name] = []Node{n}
		}
	}
	return plan
}

// Members reports whether node is in slot's group under this plan.
func (p ReloadPlan) Members(slot, node string) bool {
	for _, n := range p.Candidates {
		if n.Name == node {
			return true
		}
	}
	for _, n := range p.Extras[slot] {
		if n.Name == node {
			return true
		}
	}
	return false
}

// Retained counts the distinct nodes kept for leased slots.
func (p ReloadPlan) Retained() int {
	seen := map[string]bool{}
	for _, nodes := range p.Extras {
		for _, n := range nodes {
			seen[n.Name] = true
		}
	}
	return len(seen)
}

// nodes returns every node the plan defines, by name.
func (p ReloadPlan) nodes() map[string]Node {
	out := make(map[string]Node, len(p.Candidates))
	for _, n := range p.Candidates {
		out[n.Name] = n
	}
	for _, extras := range p.Extras {
		for _, n := range extras {
			out[n.Name] = n
		}
	}
	return out
}
