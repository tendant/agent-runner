package agentsecrets

import "strings"

// runnerCredentialNames and runnerCredentialPrefixes are agent-runner's own
// credentials (chat bot tokens, its API key, the memory repo's token, the
// scheduler database, the keys of its own cheap-LLM calls). An agent CLI
// run on the host (no sandbox) gets the runner's environment, minus these:
// the agent is given its own secrets, never the runner's. The model
// provider keys the CLI itself needs (ANTHROPIC_API_KEY, ...) stay.
var runnerCredentialNames = map[string]bool{
	"API_KEY": true, "GIT_TOKEN": true, "GIT_SSH_KEY": true,
}
var runnerCredentialPrefixes = []string{
	"STREAM_", "TELEGRAM_", "WECHAT_", "MEMORY_GIT_", "SCHEDULER_",
	"FAST_LLM_", "ANALYZER_", "PLANNER_", "CURATOR_",
}

// IsRunnerCredential reports whether name is one of agent-runner's own
// credentials, kept from host-run agents.
func IsRunnerCredential(name string) bool {
	if runnerCredentialNames[name] {
		return true
	}
	for _, p := range runnerCredentialPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// HostAgentEnv is environ (KEY=VALUE pairs, os.Environ()) without
// agent-runner's own credentials: the environment of an agent CLI run on
// the host.
func HostAgentEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !IsRunnerCredential(name) {
			out = append(out, kv)
		}
	}
	return out
}
