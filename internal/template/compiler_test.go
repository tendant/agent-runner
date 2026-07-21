package template

import (
	"strings"
	"testing"
)

func TestCompile_EmptyInput(t *testing.T) {
	result := Compile(PromptInput{})
	if result != "" {
		t.Errorf("expected empty string for empty input, got %q", result)
	}
}

func TestCompile_SystemInstructionsFirst(t *testing.T) {
	input := PromptInput{
		SystemInstructions: "You are an agent.",
		CurrentRequest:     "do something",
	}
	result := Compile(input)
	idxSys := strings.Index(result, "You are an agent.")
	idxReq := strings.Index(result, "do something")
	if idxSys < 0 || idxReq < 0 {
		t.Fatalf("both sections should be present, got: %q", result)
	}
	if idxSys >= idxReq {
		t.Errorf("system instructions should appear before current request")
	}
}

func TestCompile_MemorySectionsAsBlocks(t *testing.T) {
	input := PromptInput{
		Retrieval: Retrieval{
			Files: []MemoryFile{
				{Name: "User Preferences", Content: "prefer Go"},
				{Name: "Project Summary", Content: "a web server"},
			},
		},
	}
	result := Compile(input)
	if !strings.Contains(result, "## User Preferences\n\nprefer Go") {
		t.Errorf("expected '## User Preferences\\n\\nprefer Go' in result, got: %q", result)
	}
	if !strings.Contains(result, "## Project Summary\n\na web server") {
		t.Errorf("expected '## Project Summary\\n\\na web server' in result, got: %q", result)
	}
}

func TestCompile_MemorySectionsInOrder(t *testing.T) {
	input := PromptInput{
		Retrieval: Retrieval{
			Files: []MemoryFile{
				{Name: "First", Content: "content A"},
				{Name: "Second", Content: "content B"},
			},
		},
	}
	result := Compile(input)
	idxA := strings.Index(result, "content A")
	idxB := strings.Index(result, "content B")
	if idxA < 0 || idxB < 0 {
		t.Fatalf("both memory sections should be present")
	}
	if idxA >= idxB {
		t.Errorf("memory sections should appear in order: A before B")
	}
}

func TestCompile_EmptyMemoryFilesSkipped(t *testing.T) {
	input := PromptInput{
		Retrieval: Retrieval{
			Files: []MemoryFile{
				{Name: "Non-empty", Content: "has content"},
				{Name: "Empty", Content: ""},
			},
		},
	}
	result := Compile(input)
	if !strings.Contains(result, "## Non-empty") {
		t.Error("non-empty memory file should appear")
	}
	if strings.Contains(result, "## Empty") {
		t.Error("empty memory file should be skipped")
	}
}

func TestCompile_NoConversation_OmitsSection(t *testing.T) {
	input := PromptInput{
		SystemInstructions: "instructions",
		CurrentRequest:     "request",
	}
	result := Compile(input)
	if strings.Contains(result, "## Recent Conversation") {
		t.Error("Recent Conversation section should be omitted when empty")
	}
}

func TestCompile_CurrentRequestLast(t *testing.T) {
	input := PromptInput{
		SystemInstructions: "system",
		Retrieval: Retrieval{
			Files: []MemoryFile{{Name: "Memory", Content: "mem"}},
		},
		CurrentRequest: "do task",
	}
	result := Compile(input)
	if !strings.Contains(result, "## Current Request\n\ndo task") {
		t.Errorf("current request should appear under '## Current Request', got: %q", result)
	}
	idxMem := strings.Index(result, "## Memory")
	idxReq := strings.Index(result, "## Current Request")
	if idxMem >= idxReq {
		t.Error("memory section should appear before current request")
	}
}

func TestCompile_VarsSubstitutedInAllFields(t *testing.T) {
	vars := map[string]string{
		"NAME":    "Alice",
		"PROJECT": "myapp",
	}
	input := PromptInput{
		SystemInstructions: "Hello {{NAME}}",
		Retrieval: Retrieval{
			Files: []MemoryFile{{Name: "Info", Content: "project is {{PROJECT}}"}},
		},
		CurrentRequest: "task for {{NAME}}",
		Vars:           vars,
	}
	result := Compile(input)
	if !strings.Contains(result, "Hello Alice") {
		t.Error("var substitution in SystemInstructions")
	}
	if !strings.Contains(result, "project is myapp") {
		t.Error("var substitution in memory file content")
	}
	if !strings.Contains(result, "task for Alice") {
		t.Error("var substitution in CurrentRequest")
	}
}

func TestCompile_SectionsSeparatedByDoubleNewline(t *testing.T) {
	input := PromptInput{
		SystemInstructions: "system",
		CurrentRequest:     "request",
	}
	result := Compile(input)
	if !strings.Contains(result, "system\n\n## Current Request\n\nrequest") {
		t.Errorf("sections should be separated by \\n\\n, got: %q", result)
	}
}
