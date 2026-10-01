//go:build unit

package vpngate

import (
	"reflect"
	"testing"
)

func knownNodes(names ...string) map[string]Node {
	out := map[string]Node{}
	for _, n := range testNodes(names...) {
		out[n.Name] = n
	}
	return out
}

func TestPlanReloadKeepsAVanishedNodeForItsLeasedSlotOnly(t *testing.T) {
	slots := MakeSlots(2, 20001)
	plan := PlanReload(knownNodes("A", "B", "OLD"), testNodes("A", "B", "N"),
		slots[:1], map[string]string{"slot01": "OLD", "slot02": "OLD"})

	if got := nodeNames(plan.Extras["slot01"]); !reflect.DeepEqual(got, []string{"OLD"}) {
		t.Fatalf("slot01 extras = %v, want [OLD]", got)
	}
	if _, ok := plan.Extras["slot02"]; ok {
		t.Fatalf("slot02 has no lease, so it must not keep OLD: %v", plan.Extras)
	}
	if !plan.Members("slot01", "OLD") || plan.Members("slot02", "OLD") {
		t.Fatal("OLD must be in slot01's group only")
	}
	if !plan.Members("slot02", "N") {
		t.Fatal("every candidate must be in every group")
	}
	if plan.Retained() != 1 {
		t.Fatalf("Retained() = %d, want 1", plan.Retained())
	}
}

func TestPlanReloadKeepsNothingWhenTheNodeIsStillListed(t *testing.T) {
	slots := MakeSlots(1, 20001)
	plan := PlanReload(knownNodes("A", "B"), testNodes("A", "C"), slots, map[string]string{"slot01": "A"})
	if len(plan.Extras) != 0 || plan.Retained() != 0 {
		t.Fatalf("extras = %v, want none: A is still a candidate", plan.Extras)
	}
}

func TestPlanReloadIgnoresANodeItDoesNotKnow(t *testing.T) {
	slots := MakeSlots(1, 20001)
	plan := PlanReload(knownNodes("A"), testNodes("B"), slots, map[string]string{"slot01": "GHOST"})
	if len(plan.Extras) != 0 {
		t.Fatalf("extras = %v: a node without a known definition cannot be kept", plan.Extras)
	}
}

func TestPlanReloadCountsASharedRetainedNodeOnce(t *testing.T) {
	slots := MakeSlots(2, 20001)
	plan := PlanReload(knownNodes("A", "OLD"), testNodes("A"), slots,
		map[string]string{"slot01": "OLD", "slot02": "OLD"})
	if plan.Retained() != 1 || !plan.Members("slot01", "OLD") || !plan.Members("slot02", "OLD") {
		t.Fatalf("plan = %+v, want OLD kept in both groups and counted once", plan)
	}
}

func nodeNames(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}
