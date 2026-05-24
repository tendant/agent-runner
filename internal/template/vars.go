package template

import "strings"

const (
	VarMessage    = "MESSAGE"
	VarDate       = "DATE"
	VarIteration  = "ITERATION"
	VarProjectDir = "PROJECT_DIR"
	VarMemoryDir  = "MEMORY_DIR"
	VarRunnerURL  = "RUNNER_URL"
	VarAPIKey     = "API_KEY"
	VarRepos      = "REPOS"
)

func substituteVars(text string, vars map[string]string) string {
	for k, v := range vars {
		text = strings.ReplaceAll(text, "{{"+k+"}}", v)
	}
	return text
}
