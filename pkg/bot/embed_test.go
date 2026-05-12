package bot

import (
	"reflect"
	"testing"

	"chatbot-alpha-1/pkg/github"
)

func TestProgressBar(t *testing.T) {
	cases := []struct {
		name        string
		done, total int
		want        string
	}{
		{"zero total", 0, 0, "—"},
		{"none done", 0, 10, "░░░░░░░░░░ 0 / 10 (0%)"},
		{"half done", 5, 10, "█████░░░░░ 5 / 10 (50%)"},
		{"all done", 10, 10, "██████████ 10 / 10 (100%)"},
		{"seven of fourteen", 7, 14, "█████░░░░░ 7 / 14 (50%)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := progressBar(c.done, c.total)
			if got != c.want {
				t.Fatalf("progressBar(%d, %d) = %q, want %q", c.done, c.total, got, c.want)
			}
		})
	}
}

func TestGroupChecks(t *testing.T) {
	checks := []github.CheckRun{
		{Name: "unit", Status: "completed", Conclusion: "success"},
		{Name: "unit", Status: "completed", Conclusion: "success"},
		{Name: "unit", Status: "completed", Conclusion: "neutral"},
		{Name: "unit", Status: "completed", Conclusion: "skipped"},
		{Name: "unit", Status: "completed", Conclusion: "success"},
		{Name: "integration", Status: "in_progress"},
		{Name: "deploy", Status: "queued"},
		{Name: "rule", Status: "completed", Conclusion: "failure"},
		{Name: "lint", Status: "completed", Conclusion: "cancelled"},
		{Name: "slow", Status: "completed", Conclusion: "timed_out"},
		{Name: "manual", Status: "completed", Conclusion: "action_required"},
	}

	passed, running, failed := groupChecks(checks)

	wantPassed := []groupedCheck{{Name: "unit", Count: 5}}
	wantRunning := []groupedCheck{{Name: "deploy", Count: 1}, {Name: "integration", Count: 1}}
	wantFailed := []groupedCheck{
		{Name: "lint", Count: 1},
		{Name: "manual", Count: 1},
		{Name: "rule", Count: 1},
		{Name: "slow", Count: 1},
	}
	if !reflect.DeepEqual(passed, wantPassed) {
		t.Fatalf("passed = %#v, want %#v", passed, wantPassed)
	}
	if !reflect.DeepEqual(running, wantRunning) {
		t.Fatalf("running = %#v, want %#v", running, wantRunning)
	}
	if !reflect.DeepEqual(failed, wantFailed) {
		t.Fatalf("failed = %#v, want %#v", failed, wantFailed)
	}
}
