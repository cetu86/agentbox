package main

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testPlan(t *testing.T) *plan {
	t.Helper()
	p, err := parsePlan([]byte(`{
  "name": "demo",
  "tasks": [
    {"id": "a", "prompt": "do a"},
    {"id": "b", "prompt": "do b"},
    {"id": "c", "prompt": "do c", "depends_on": ["a", "b"], "auto_approve": true},
    {"id": "d", "prompt": "do d", "depends_on": ["c"]}
  ]
}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParsePlanErrors(t *testing.T) {
	cases := map[string]struct{ json, want string }{
		"syntax":      {`{`, "parse plan"},
		"unknown key": {`{"name":"x","tasks":[{"id":"a","prompt":"p","bogus":1}]}`, "bogus"},
		"bad name":    {`{"name":"Bad Name","tasks":[{"id":"a","prompt":"p"}]}`, "plan name"},
		"no tasks":    {`{"name":"x","tasks":[]}`, "no tasks"},
		"bad id":      {`{"name":"x","tasks":[{"id":"A","prompt":"p"}]}`, "task id"},
		"duplicate":   {`{"name":"x","tasks":[{"id":"a","prompt":"p"},{"id":"a","prompt":"p"}]}`, "duplicate"},
		"empty":       {`{"name":"x","tasks":[{"id":"a","prompt":"  "}]}`, "empty prompt"},
		"bad net":     {`{"name":"x","tasks":[{"id":"a","prompt":"p","net":"wild"}]}`, "net"},
		"self":        {`{"name":"x","tasks":[{"id":"a","prompt":"p","depends_on":["a"]}]}`, "depends on itself"},
		"twice":       {`{"name":"x","tasks":[{"id":"a","prompt":"p"},{"id":"b","prompt":"p","depends_on":["a","a"]}]}`, "twice"},
		"unknown dep": {`{"name":"x","tasks":[{"id":"a","prompt":"p","depends_on":["z"]}]}`, "unknown task"},
		"cycle": {`{"name":"x","tasks":[{"id":"a","prompt":"p","depends_on":["c"]},
			{"id":"b","prompt":"p","depends_on":["a"]},{"id":"c","prompt":"p","depends_on":["b"]}]}`, "dependency cycle"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parsePlan([]byte(tc.json))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	_, err := parsePlan([]byte(`{"name":"x","tasks":[{"id":"a","prompt":""},{"id":"b","prompt":"p","depends_on":["z"]}]}`))
	if err == nil || !strings.Contains(err.Error(), "empty prompt") || !strings.Contains(err.Error(), "unknown task") {
		t.Fatalf("expected both problems, got %v", err)
	}
}

func TestCycleIsReported(t *testing.T) {
	_, err := parsePlan([]byte(`{"name":"x","tasks":[{"id":"a","prompt":"p","depends_on":["b"]},{"id":"b","prompt":"p","depends_on":["a"]}]}`))
	if err == nil || !strings.Contains(err.Error(), "a -> b -> a") {
		t.Fatalf("err = %v", err)
	}
}

func TestLevelsAndDependents(t *testing.T) {
	p := testPlan(t)
	want := [][]string{{"a", "b"}, {"c"}, {"d"}}
	if got := p.levels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("levels = %v, want %v", got, want)
	}
	if got := p.dependents("a"); !reflect.DeepEqual(got, []string{"c", "d"}) {
		t.Fatalf("dependents(a) = %v", got)
	}
	if got := p.dependents("d"); len(got) != 0 {
		t.Fatalf("dependents(d) = %v", got)
	}
}

func TestReadyTasks(t *testing.T) {
	p := testPlan(t)
	s := newPlanState()
	now := time.Now()

	if got := p.readyTasks(s, 0); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("initial ready = %v", got)
	}
	if got := p.readyTasks(s, 1); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("limited ready = %v", got)
	}

	s.set("a", taskRunning, now)
	if got := p.readyTasks(s, 1); len(got) != 0 {
		t.Fatalf("slot should be taken, got %v", got)
	}
	if got := p.readyTasks(s, 2); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("ready with one running = %v", got)
	}

	// A task awaiting review frees its slot but does not unlock dependents.
	s.set("a", taskReview, now)
	s.set("b", taskAccepted, now)
	if got := p.readyTasks(s, 1); len(got) != 0 {
		t.Fatalf("c must wait for a, got %v", got)
	}

	s.set("a", taskAccepted, now)
	if got := p.readyTasks(s, 1); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("ready after a,b accepted = %v", got)
	}

	// A resolver agent occupies a slot.
	s.set("c", taskConflict, now)
	if got := p.readyTasks(s, 1); len(got) != 0 {
		t.Fatalf("conflict should hold a slot, got %v", got)
	}
}

func TestBlockedPropagation(t *testing.T) {
	p := testPlan(t)
	s := newPlanState()
	s.set("a", taskBlocked, time.Now())
	if !p.isBlockedByDeps(s, "c") || !p.isBlockedByDeps(s, "d") {
		t.Fatal("c and d should be unreachable")
	}
	if p.isBlockedByDeps(s, "b") {
		t.Fatal("b is independent of a")
	}
	if got := p.readyTasks(s, 0); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("ready = %v", got)
	}
	counts := p.planSummary(s)
	want := map[string]int{taskBlocked: 1, "ready": 1, "unreachable": 2}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("summary = %v, want %v", counts, want)
	}
}

func TestIsComplete(t *testing.T) {
	p := testPlan(t)
	s := newPlanState()
	if p.isComplete(s) {
		t.Fatal("fresh plan is not complete")
	}
	for _, task := range p.Tasks {
		s.set(task.ID, taskAccepted, time.Now())
	}
	if !p.isComplete(s) {
		t.Fatal("all accepted should be complete")
	}
}

func TestPlanRoundTrip(t *testing.T) {
	root := t.TempDir()
	p := testPlan(t)
	if err := savePlan(root, p); err != nil {
		t.Fatal(err)
	}
	got, err := loadPlan(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", got, p)
	}
	if !got.task("c").AutoApprove {
		t.Fatal("auto_approve lost")
	}
}

func TestSavePlanRejectsInvalid(t *testing.T) {
	root := t.TempDir()
	if err := savePlan(root, &plan{Name: "x"}); err == nil {
		t.Fatal("expected validation error")
	}
	if _, err := os.Stat(planPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid plan must not be written, stat err = %v", err)
	}
}

func TestPlanStateRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := loadPlanState(root)
	if err != nil || len(s.Tasks) != 0 {
		t.Fatalf("missing state should be empty: %v %v", s, err)
	}
	ts := s.set("a", taskRunning, time.Unix(100, 0).UTC())
	ts.Box = "a"
	ts.Attempts = 2
	if err := savePlanState(root, s); err != nil {
		t.Fatal(err)
	}
	got, err := loadPlanState(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("state mismatch:\n%+v\n%+v", got, s)
	}
	if got.status("zzz") != taskPending {
		t.Fatal("unknown task should be pending")
	}
}

func TestLoadPlanStateCorrupt(t *testing.T) {
	root := t.TempDir()
	if err := writeFileAtomic(planStatePath(root), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlanState(root); err == nil {
		t.Fatal("expected parse error")
	}
}
