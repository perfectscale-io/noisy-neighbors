package wall

import (
	"testing"
	"time"

	"github.com/perfectscale-io/noisy-neighbors/internal/analyze"
)

const mebi = 1024 * 1024

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

// TestAssignRoles covers the tile treatment: the findings the analyzer supplies
// win, stranding is computed locally, and the ordering holds.
func TestAssignRoles(t *testing.T) {
	th := analyze.DefaultThresholds()

	cases := []struct {
		name  string
		pod   Pod
		found []Role
		want  Role
	}{
		{
			name: "an aggressor finding makes an aggressor tile",
			pod: Pod{
				ReqMem: 64 * mebi, ReqCPU: 50, HasMemRequest: true, HasCPURequest: true,
				UseMem: i64(701 * mebi), UseCPU: i64(999), QoS: "Burstable",
			},
			found: []Role{RoleAggressor},
			want:  RoleAggressor,
		},
		{
			name: "stranded needs no finding: the report has no per-pod one",
			pod: Pod{
				ReqMem: 1024 * mebi, ReqCPU: 500, HasMemRequest: true, HasCPURequest: true,
				UseMem: i64(700 * 1024), UseCPU: i64(1), QoS: "Burstable",
			},
			want: RoleStranded,
		},
		{
			// team-search after right-sizing: 10m CPU requested, metrics-server
			// reports 0m, memory request matches use. Zero usage used to count
			// as stranding, which painted a fixed tenant as wasteful.
			name: "zero usage is not stranding, matching the report",
			pod: Pod{
				ReqMem: 159 * mebi, ReqCPU: 10, HasMemRequest: true, HasCPURequest: true,
				UseMem: i64(161 * mebi), UseCPU: i64(0), QoS: "Burstable",
			},
			want: RoleOK,
		},
		{
			name: "throttled beats stranded: being held under a limit is worse than idling",
			pod: Pod{
				ReqMem: 32 * mebi, ReqCPU: 50, HasMemRequest: true, HasCPURequest: true,
				UseMem: i64(340 * 1024), UseCPU: i64(50), QoS: "Burstable",
				ThrottlePct: f64(99),
			},
			found: []Role{RoleThrottled},
			want:  RoleThrottled,
		},
		{
			name: "oom outranks everything: being killed is the most urgent state",
			pod: Pod{
				ReqMem: 128 * mebi, ReqCPU: 100, HasMemRequest: true, HasCPURequest: true,
				UseMem: i64(18 * mebi), UseCPU: i64(12), QoS: "Guaranteed",
				ThrottlePct: f64(99), OOMAgoSec: f64(8),
			},
			found: []Role{RoleThrottled, RoleOOM},
			want:  RoleOOM,
		},
		{
			name: "besteffort comes off QoS even with no finding",
			pod:  Pod{QoS: "BestEffort", UseMem: i64(352 * 1024), UseCPU: i64(1)},
			want: RoleBestEffort,
		},
		{
			name:  "an unrequested-consumption finding outranks BestEffort",
			pod:   Pod{QoS: "BestEffort", UseMem: i64(2048 * mebi), UseCPU: i64(1500)},
			found: []Role{RoleAggressor},
			want:  RoleAggressor,
		},
		{
			name: "no usage tier: cannot be judged, so it is not judged",
			pod: Pod{
				ReqMem: 1024 * mebi, ReqCPU: 500, HasMemRequest: true, HasCPURequest: true,
				QoS: "Burstable",
			},
			want: RoleOK,
		},
		{
			name: "an aggressor is never also drawn as stranded",
			pod: Pod{
				ReqMem: 64 * mebi, ReqCPU: 500, HasMemRequest: true, HasCPURequest: true,
				UseMem: i64(701 * mebi), UseCPU: i64(1), QoS: "Burstable",
			},
			found: []Role{RoleAggressor},
			want:  RoleAggressor,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.pod
			assignRoles(&p, th, tc.found)
			if p.Role != tc.want {
				t.Errorf("role = %q, want %q (roles: %v)", p.Role, tc.want, p.Roles)
			}
		})
	}
}

// TestTileRolesMatchFindings is the regression test for the bug this package
// shipped with: the wall re-derived the aggressor rule instead of using the
// analyzer's, gating on the size of the request where the analyzer gates on the
// size of the usage. The lab's headline aggressor asks for 64Mi and uses 700Mi,
// which is below the analyzer's memory floor as a request and far above it as
// usage, so the tile rendered neutral while the panel beside it called it an
// aggressor. Anything the report names must be reflected on the tile.
func TestTileRolesMatchFindings(t *testing.T) {
	th := analyze.DefaultThresholds()
	cluster := Synthetic{Start: time.Now()}.Frame(60*time.Second, 30)
	rep := analyze.Run(cluster, th)
	snap := Build(cluster, rep, th, "test")

	tiles := map[string]*Pod{}
	for _, n := range snap.Nodes {
		for _, p := range n.Pods {
			tiles[p.Key()] = p
		}
	}

	if len(snap.Aggressors) == 0 {
		t.Fatal("the scripted run produced no aggressor findings; the fixture is wrong")
	}

	for _, f := range snap.Aggressors {
		p := tiles[f.Workload]
		if p == nil {
			t.Errorf("finding names %q but no tile carries that key", f.Workload)
			continue
		}
		if p.Role != RoleAggressor {
			t.Errorf("%s is an aggressor in the report but its tile is %q: %s",
				f.Workload, p.Role, f.Message)
		}
	}
}

// TestDiffDetectsTransitions covers the event rail. A snapshot can say a pod is
// currently bad; only a comparison can say it just went bad, and the moment it
// goes bad is what the demo exists to show.
func TestDiffDetectsTransitions(t *testing.T) {
	at := time.Now()
	frame := func(pods ...*Pod) *Snapshot {
		return &Snapshot{At: at, Nodes: []*Node{{Name: "node-a", Pods: pods}}}
	}

	t.Run("restart with a fresh oomkill reads as an OOMKill", func(t *testing.T) {
		prev := frame(&Pod{Namespace: "team-search", Name: "api-1", Restarts: 0})
		cur := frame(&Pod{Namespace: "team-search", Name: "api-1", Restarts: 1, OOMAgoSec: f64(2)})
		Diff(prev, cur)
		if len(cur.Events) != 1 || cur.Events[0].Kind != EventOOMKill {
			t.Fatalf("events = %+v, want one oomkill", cur.Events)
		}
	})

	t.Run("restart without an oomkill is only a restart", func(t *testing.T) {
		prev := frame(&Pod{Namespace: "team-search", Name: "api-1", Restarts: 1})
		cur := frame(&Pod{Namespace: "team-search", Name: "api-1", Restarts: 2})
		Diff(prev, cur)
		if len(cur.Events) != 1 || cur.Events[0].Kind != EventRestart {
			t.Fatalf("events = %+v, want one restart", cur.Events)
		}
	})

	t.Run("a steady bad state emits nothing", func(t *testing.T) {
		p := func() *Pod {
			return &Pod{Namespace: "team-billing", Name: "worker-1", ThrottlePct: f64(99)}
		}
		prev, cur := frame(p()), frame(p())
		Diff(prev, cur)
		if len(cur.Events) != 0 {
			t.Fatalf("events = %+v, want none: nothing changed", cur.Events)
		}
	})

	t.Run("crossing into throttling emits once", func(t *testing.T) {
		prev := frame(&Pod{Namespace: "team-billing", Name: "worker-1"})
		cur := frame(&Pod{Namespace: "team-billing", Name: "worker-1", ThrottlePct: f64(99)})
		Diff(prev, cur)
		if len(cur.Events) != 1 || cur.Events[0].Kind != EventThrottle {
			t.Fatalf("events = %+v, want one throttle", cur.Events)
		}
	})

	t.Run("the aggressor arriving is an event; system pods are not", func(t *testing.T) {
		prev := frame()
		cur := frame(
			&Pod{Namespace: "team-payments", Name: "ingest-1"},
			&Pod{Namespace: "kube-system", Name: "aws-node-1", System: true},
		)
		Diff(prev, cur)
		if len(cur.Events) != 1 || cur.Events[0].Kind != EventPodAdded ||
			cur.Events[0].Namespace != "team-payments" {
			t.Fatalf("events = %+v, want one added for team-payments", cur.Events)
		}
	})

	t.Run("no previous frame means no events, not a wall of them", func(t *testing.T) {
		cur := frame(&Pod{Namespace: "team-payments", Name: "ingest-1", Restarts: 4})
		Diff(nil, cur)
		if len(cur.Events) != 0 {
			t.Fatalf("events = %+v, want none on the first frame", cur.Events)
		}
	})
}

// TestSyntheticTimeline checks the scripted run actually produces the arc the
// demo narrates, so a dry run cannot pass while the story quietly does not.
func TestSyntheticTimeline(t *testing.T) {
	s := Synthetic{Start: time.Now()}

	calm := s.Frame(10*time.Second, -1)
	for _, n := range calm.Nodes {
		for _, p := range n.Pods {
			if p.Namespace == "team-payments" {
				t.Fatal("team-payments is present before the button; the demo opens calm")
			}
		}
	}

	pressed := s.Frame(10*time.Second, 20)
	var found bool
	for _, n := range pressed.Nodes {
		for _, p := range n.Pods {
			if p.Namespace != "team-payments" {
				continue
			}
			found = true
			if p.Usage == nil || p.Usage.MemBytes <= p.Requests.MemBytes {
				t.Errorf("aggressor memory %v does not exceed its %v request",
					p.Usage, p.Requests)
			}
			if p.Containers[0].HasMemLimit {
				t.Error("the aggressor declares a memory limit; it must declare none")
			}
		}
	}
	if !found {
		t.Fatal("team-payments missing after the button")
	}

	// search must be killed by its own limit on its own clock, with no
	// aggressor present: the wall must never imply a causation the lab lacks.
	late := s.Frame(time.Duration(firstOOMAtS+2)*time.Second, -1)
	for _, n := range late.Nodes {
		for _, p := range n.Pods {
			if p.Namespace == "team-search" {
				if p.LastOOMKill() == nil {
					t.Error("team-search has not OOMKilled after its first cycle")
				}
				if p.Restarts() == 0 {
					t.Error("team-search restart count did not advance")
				}
			}
		}
	}
}
