package config

import (
	"strings"
	"testing"
)

// TestResolveAgentDialsHonorsExplicitZero is the whole point of the pointer
// conversion: a written 0 is a value with its own meaning, and must not be
// filled in with the default the way an omitted field is.
func TestResolveAgentDialsHonorsExplicitZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		agent Agent
		step  Step
		want  int
	}{
		{"neither set takes the default", Agent{}, Step{}, defaultMaxAgentTurns},
		{"the agent's applies when the step is silent", Agent{MaxTurns: intPtr(12)}, Step{}, 12},
		{"the step's wins", Agent{MaxTurns: intPtr(12)}, Step{MaxTurns: intPtr(40)}, 40},
		{"the agent's 0 removes the cap", Agent{MaxTurns: intPtr(0)}, Step{}, 0},
		{"the step's 0 removes a cap the agent set", Agent{MaxTurns: intPtr(12)}, Step{MaxTurns: intPtr(0)}, 0},
		// The reverse direction matters just as much: an uncapped agent with
		// one step that wants a leash must get the leash on that step alone.
		{"the step's cap beats the agent's 0", Agent{MaxTurns: intPtr(0)}, Step{MaxTurns: intPtr(5)}, 5},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			agent := test.agent
			agent.Name = "a"
			agent.Source = AgentSource{Model: "openai/gpt-4o"}

			step := test.step
			step.Agent = "a"

			cfg := &Config{Agents: []Agent{agent}}

			ri, err := cfg.ResolveAgentInvocation(step)
			if err != nil {
				t.Fatal(err)
			}

			if ri.MaxTurns != test.want {
				t.Errorf("MaxTurns = %d, want %d", ri.MaxTurns, test.want)
			}
		})
	}
}

// TestResolveAgentTimeoutAndAttemptsPrecedence pins the two dials an agents:
// entry gained. Both follow max_turns:' existing order rather than inventing
// one — step, then entry, then package default.
func TestResolveAgentTimeoutAndAttemptsPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		agent        Agent
		step         Step
		wantTimeout  string
		wantAttempts int
	}{
		{"neither set", Agent{}, Step{}, "", defaultAgentAttempts},
		{"the entry's apply", Agent{Timeout: "20m", Attempts: intPtr(5)}, Step{}, "20m", 5},
		{"the step's win", Agent{Timeout: "20m", Attempts: intPtr(5)}, Step{Timeout: "45m", Attempts: intPtr(2)}, "45m", 2},
		// "0" is a deadline the step asked for, not an absent one, so it must
		// not fall through to the entry's 20m.
		{"the step's explicit no-deadline wins", Agent{Timeout: "20m"}, Step{Timeout: "0"}, "0", defaultAgentAttempts},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			agent := test.agent
			agent.Name = "a"
			agent.Source = AgentSource{Model: "openai/gpt-4o"}

			step := test.step
			step.Agent = "a"

			cfg := &Config{Agents: []Agent{agent}}

			ri, err := cfg.ResolveAgentInvocation(step)
			if err != nil {
				t.Fatal(err)
			}

			if ri.Timeout != test.wantTimeout {
				t.Errorf("Timeout = %q, want %q", ri.Timeout, test.wantTimeout)
			}

			if ri.Attempts != test.wantAttempts {
				t.Errorf("Attempts = %d, want %d", ri.Attempts, test.wantAttempts)
			}
		})
	}
}

// TestAttemptsZeroIsRejected covers the one dial deliberately outside the
// convention. The message has to explain itself: 0 is the "no limit" spelling
// everywhere else on the page, so an author who writes it here is guessing at
// a rule, not making a typo.
func TestAttemptsZeroIsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *Config
	}{
		{"on a step", &Config{Jobs: []Job{{Name: "j", Plan: []Step{{Task: "t", Run: "true", Attempts: intPtr(0)}}}}}},
		{"on an agents: entry", &Config{Agents: []Agent{{Name: "a", Source: AgentSource{Model: "openai/gpt-4o"}, Attempts: intPtr(0)}}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var err error
			if len(test.cfg.Agents) > 0 {
				err = test.cfg.validateAgentDials()
			} else {
				err = test.cfg.validateAttempts()
			}

			if err == nil {
				t.Fatal("attempts: 0 loaded cleanly, but unbounded retry has no backstop")
			}

			if !strings.Contains(err.Error(), "does not mean unlimited") {
				t.Errorf("error = %v, want one saying why 0 is not the no-limit spelling here", err)
			}
		})
	}
}

// TestStepTimeoutZeroOnlyOnAgentSteps pins the asymmetry. It is not a special
// case for agent steps so much as the absence of one: they are the only kind
// whose empty timeout: does not already mean "no deadline", so they are the
// only kind where 0 has anything left to say.
func TestStepTimeoutZeroOnlyOnAgentSteps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		step    Step
		wantErr bool
	}{
		{"0 on an agent step means no deadline", Step{Agent: "a", Timeout: "0"}, false},
		{"0s likewise", Step{Agent: "a", Timeout: "0s"}, false},
		{"0 on a task step is rejected", Step{Task: "t", Run: "true", Timeout: "0"}, true},
		{"a positive duration is fine anywhere", Step{Task: "t", Run: "true", Timeout: "30s"}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{Jobs: []Job{{Name: "j", Plan: []Step{test.step}}}}

			err := cfg.validateStepTimeouts()
			if (err != nil) != test.wantErr {
				t.Errorf("validateStepTimeouts() = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
