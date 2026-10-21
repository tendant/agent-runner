package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Model keys without the sandbox. agent-runner's model keys (/config set
// DEEPSEEK_API_KEY ...) would otherwise reach an agent CLI under the
// standard names, which the CLI and every program it runs for the agent
// share: an agent secret of the same name (an app's own DeepSeek key) would
// replace the CLI's model key, or the reverse. So each CLI gets agent-
// runner's key by a route that outranks the environment, and the standard
// names are left to the agent:
//
//   - pi: --api-key for the provider it runs (above auth.json and env)
//   - codex: a provider defined with -c whose key is AGENT_RUNNER_OPENAI_API_KEY
//   - opencode: OPENCODE_CONFIG_CONTENT, apiKey "{env:AGENT_RUNNER_<KEY>}"
//   - claude: none exists (its environment outranks apiKeyHelper, and the
//     programs it runs get its environment), so ANTHROPIC_API_KEY stays
//     agent-runner's while the agent CLI is claude (agentsecrets.EnvFor)

// modelKeys are the provider keys agent-runner manages for agent CLIs, by
// the provider name pi and opencode use.
var modelKeys = map[string]string{
	"ANTHROPIC_API_KEY": "anthropic",
	"OPENAI_API_KEY":    "openai",
	"DEEPSEEK_API_KEY":  "deepseek",
}

// runnerKeyEnv is the variable a CLI reads agent-runner's key name from.
func runnerKeyEnv(name string) string { return "AGENT_RUNNER_" + name }

// routedCLIs are the agent CLIs whose model key goes by its own route.
var routedCLIs = []string{"pi", "codex", "opencode", "claude"}

// hostModelRoute takes agent-runner's model keys out of a host-run agent
// CLI's environment and returns the CLI's arguments and extra environment
// that give them to it by its own route. Other programs are left as is.
func hostModelRoute(name string, args, env []string) ([]string, []string) {
	cli := filepath.Base(name)
	if !slices.Contains(routedCLIs, cli) {
		return args, env
	}
	// Where there is no route to give a key by, it stays where the CLI
	// looks for it: pi without --provider (its default comes from its own
	// settings), codex set to another provider in its config.toml (which may
	// read OPENAI_API_KEY itself), claude's ANTHROPIC_API_KEY.
	keep := func(k string) bool {
		switch {
		case cli == "claude":
			return k == "ANTHROPIC_API_KEY"
		case cli == "pi":
			return flagValue(args, "--provider") == ""
		case cli == "codex":
			return k == "OPENAI_API_KEY" && !codexUsesOpenAI()
		}
		return false
	}
	keys := map[string]string{}
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, isKey := modelKeys[k]; isKey && !keep(k) {
			if v != "" {
				keys[k] = v
			}
			continue
		}
		kept = append(kept, kv)
	}
	switch cli {
	case "pi":
		provider := flagValue(args, "--provider")
		for k, p := range modelKeys {
			if p == provider && keys[k] != "" && flagValue(args, "--api-key") == "" {
				args = append(append([]string{}, args...), "--api-key", keys[k])
			}
		}
	case "codex":
		if key := keys["OPENAI_API_KEY"]; key != "" {
			base := strings.TrimSuffix(os.Getenv("OPENAI_BASE_URL"), "/")
			if base == "" {
				base = "https://api.openai.com/v1"
			}
			opts := []string{
				"-c", `model_provider="agent-runner-openai"`,
				"-c", `model_providers.agent-runner-openai={name="OpenAI",base_url=` + tomlString(base) + `,env_key="` + runnerKeyEnv("OPENAI_API_KEY") + `",wire_api="responses"}`,
			}
			if len(args) > 0 && args[0] == "exec" {
				args = append(append([]string{"exec"}, opts...), args[1:]...)
			} else {
				args = append(opts, args...)
			}
			kept = append(kept, runnerKeyEnv("OPENAI_API_KEY")+"="+key)
		}
	case "opencode":
		providers := map[string]any{}
		for k, v := range keys {
			providers[modelKeys[k]] = map[string]any{"options": map[string]any{"apiKey": "{env:" + runnerKeyEnv(k) + "}"}}
			kept = append(kept, runnerKeyEnv(k)+"="+v)
		}
		if len(providers) > 0 {
			content, _ := json.Marshal(map[string]any{"provider": providers})
			kept = append(kept, "OPENCODE_CONFIG_CONTENT="+string(content))
		}
	}
	return args, kept
}

// flagValue is the value after flag in args ("" when absent).
func flagValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// codexProviderLine matches a top-level model_provider setting.
var codexProviderLine = regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"([^"]*)"`)

// codexUsesOpenAI reports whether codex's own config leaves it on OpenAI's
// API (no other model_provider chosen): only then is its key routed.
func codexUsesOpenAI() bool {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return true
		}
		home = filepath.Join(h, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return true
	}
	m := codexProviderLine.FindSubmatch(data)
	return m == nil || string(m[1]) == "openai"
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid TOML basic string here
	return string(b)
}
