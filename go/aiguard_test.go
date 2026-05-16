package main

import "testing"

// clearAgentEnv pins all three env knobs to a known-empty baseline so a
// test is not perturbed by the CLAUDECODE the suite itself may run
// under. t.Setenv restores originals at test end.
func clearAgentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
	t.Setenv("SRV_ALLOW_AI_CLI", "")
}

func TestAIAgentDetected(t *testing.T) {
	cases := []struct {
		name, key, val string
		want           bool
	}{
		{"none", "", "", false},
		{"claudecode", "CLAUDECODE", "1", true},
		{"entrypoint", "CLAUDE_CODE_ENTRYPOINT", "cli", true},
		{"empty value is not detection", "CLAUDECODE", "  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAgentEnv(t)
			if tc.key != "" {
				t.Setenv(tc.key, tc.val)
			}
			if got := aiAgentDetected(); got != tc.want {
				t.Errorf("aiAgentDetected()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestAICLIAllowed(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Run("truthy_"+v, func(t *testing.T) {
			clearAgentEnv(t)
			t.Setenv("SRV_ALLOW_AI_CLI", v)
			if !aiCLIAllowed() {
				t.Errorf("aiCLIAllowed()=false for %q, want true", v)
			}
		})
	}
	for _, v := range []string{"", "0", "false", "off", "garbage"} {
		t.Run("falsy_"+v, func(t *testing.T) {
			clearAgentEnv(t)
			t.Setenv("SRV_ALLOW_AI_CLI", v)
			if aiCLIAllowed() {
				t.Errorf("aiCLIAllowed()=true for %q, want false", v)
			}
		})
	}
}

func TestBlockAIRemote(t *testing.T) {
	cases := []struct {
		name          string
		agent         bool
		allow         bool
		known, remote bool
		want          bool
	}{
		// Not an agent: never blocked, whatever the command.
		{"human remote run", false, false, true, true, false},
		{"human implicit run", false, false, false, false, false},
		{"human local cmd", false, false, true, false, false},

		// Agent, escape hatch on: never blocked.
		{"agent hatch remote", true, true, true, true, false},
		{"agent hatch implicit", true, true, false, false, false},

		// Agent, hatch off: remote actions + implicit run refused;
		// known local subcommands (mcp/help/config/...) allowed.
		{"agent remote subcmd", true, false, true, true, true},
		{"agent implicit run", true, false, false, false, true},
		{"agent local subcmd", true, false, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAgentEnv(t)
			if tc.agent {
				t.Setenv("CLAUDECODE", "1")
			}
			if tc.allow {
				t.Setenv("SRV_ALLOW_AI_CLI", "1")
			}
			if got := blockAIRemote(tc.known, tc.remote); got != tc.want {
				t.Errorf("blockAIRemote(known=%v,remote=%v)=%v want %v",
					tc.known, tc.remote, got, tc.want)
			}
		})
	}
}

// TestRemoteSubcommandsPolicy locks the classification: the MCP server
// entry and the common local commands must NOT be remote (or an agent
// could never start or use MCP, defeating the whole point), while the
// command-execution / transfer / stream verbs must be.
func TestRemoteSubcommandsPolicy(t *testing.T) {
	mustNotBlock := []string{"mcp", "help", "version", "config", "guard",
		"sessions", "jobs", "history", "completion", "init", "use", "status"}
	for _, n := range mustNotBlock {
		if remoteSubcommands[n] {
			t.Errorf("%q must not be a remote subcommand (agents need it / it is local)", n)
		}
	}
	mustBlock := []string{"run", "push", "pull", "sync", "tail",
		"journal", "sudo", "shell", "logs", "kill", "edit", "diff"}
	for _, n := range mustBlock {
		if !remoteSubcommands[n] {
			t.Errorf("%q must be a remote subcommand (it touches the remote)", n)
		}
	}
}
