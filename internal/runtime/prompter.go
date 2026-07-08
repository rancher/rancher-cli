package runtime

import "context"

// Prompter is the interaction port commands use for prompts and notices.
// internal/ui provides the TTY implementation; a second surface (MCP, tests)
// provides its own. Mutating commands never receive one — gates are flag-only.
type Prompter interface {
	Prompt(ctx context.Context, msg string) (string, error)
	PromptSecret(ctx context.Context, msg string) (string, error)
	ReadLine(ctx context.Context) (string, error)
	Notice(msg string)
	CanPrompt() bool
	NoInput() bool
}
