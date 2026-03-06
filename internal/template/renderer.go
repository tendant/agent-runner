package template

import (
	"fmt"
	"strings"
	"time"
)

// Render applies variable substitution and composes filtered, sorted templates
// into a single prompt string.
func Render(templates []TemplateFile, ctx TemplateContext) string {
	var sb strings.Builder

	for i, t := range templates {
		if i > 0 {
			sb.WriteString("\n\n")
		}

		body := substituteVars(t.Body, ctx)

		// Use title as section header if available
		if t.Meta.Title != "" {
			sb.WriteString(fmt.Sprintf("<!-- %s -->\n", t.Meta.Title))
		}
		sb.WriteString(body)
	}

	return sb.String()
}

// ComposePrompt is the high-level function that loads, merges, filters, sorts,
// and renders templates into a final prompt string.
func ComposePrompt(memoryDir string, phase Phase, firstRun bool, ctx TemplateContext) (string, error) {
	defaults, err := LoadDefaults()
	if err != nil {
		return "", fmt.Errorf("load default templates: %w", err)
	}

	overrides, err := LoadFromDir(memoryDir)
	if err != nil {
		return "", fmt.Errorf("load templates from %s: %w", memoryDir, err)
	}

	merged := MergeTemplates(defaults, overrides)
	filtered := FilterByPhase(merged, phase, firstRun)
	SortByPriority(filtered)

	return Render(filtered, ctx), nil
}

// NewContext creates a TemplateContext with the current date filled in.
func NewContext(message string, repos []string, iteration int, projectDir string) TemplateContext {
	return TemplateContext{
		Message:    message,
		Repos:      strings.Join(repos, ", "),
		Date:       time.Now().Format("2006-01-02"),
		Iteration:  iteration,
		ProjectDir: projectDir,
	}
}

func substituteVars(body string, ctx TemplateContext) string {
	body = strings.ReplaceAll(body, "{{MESSAGE}}", ctx.Message)
	body = strings.ReplaceAll(body, "{{REPOS}}", ctx.Repos)
	body = strings.ReplaceAll(body, "{{DATE}}", ctx.Date)
	body = strings.ReplaceAll(body, "{{ITERATION}}", fmt.Sprintf("%d", ctx.Iteration))
	body = strings.ReplaceAll(body, "{{PROJECT_DIR}}", ctx.ProjectDir)
	return body
}
