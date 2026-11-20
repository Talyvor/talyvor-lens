package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/talyvor/lens/internal/redact"
)

// agent_task.go — B62.2: THE TASK AN AGENT GIVES WITH A PAYMENT.
//
// A payment an agent asks for may carry its task: one line in the agent's own words of what it was trying to do. It is
// kept with the payment (the paying agent's postings, or the money entry of a payment from a currency account) and with
// the approval it files, and read back on both: the owner's approval, Activity and the statement export. It is what the
// agent says, never an instruction and never a fact: the rules never read it, it is no part of the fingerprint an
// approval is bound by, so a payment with any task is judged exactly as the same payment without one. Wherever Lens
// writes it in words it follows TaskSays. Key-shaped strings are cut from it (redact.Secrets) before it is kept.

// TaskMax is the most characters a task may have.
const TaskMax = 200

// TaskSays is what a task is always shown after.
const TaskSays = "The agent says: "

// ErrTask is a task that cannot be kept: one over TaskMax characters.
var ErrTask = errors.New("economy: task")

// CleanTask is task as it is kept: on one line, its runs of spaces and line breaks one space, without key-shaped strings.
// One over TaskMax characters is refused in words.
func CleanTask(task string) (string, error) {
	task = strings.Join(strings.Fields(strings.ToValidUTF8(task, "")), " ")
	if n := utf8.RuneCountInString(task); n > TaskMax {
		return "", fmt.Errorf("%w: the task is %d characters, and it may be at most %d: one line, in the agent's own words, of what the payment is for",
			ErrTask, n, TaskMax)
	}
	return redact.Secrets(task), nil
}

// TaskShown is task as Lens writes it in words: after TaskSays; "" for none.
func TaskShown(task string) string {
	if task == "" {
		return ""
	}
	return TaskSays + task
}

type agentTaskKey struct{}

type agentTask struct{ workspaceID, task string }

// WithAgentTask carries the task, already cleaned (CleanTask), of a payment workspaceID's agent asks for to the
// approval it files and the postings or money entry it writes in workspaceID — never to another workspace's side of it.
func WithAgentTask(ctx context.Context, workspaceID, task string) context.Context {
	if task == "" {
		return ctx
	}
	return context.WithValue(ctx, agentTaskKey{}, agentTask{workspaceID, task})
}

// taskIn is the task ctx carries for workspaceID; "" for none, or for another workspace's.
func taskIn(ctx context.Context, workspaceID string) string {
	t, _ := ctx.Value(agentTaskKey{}).(agentTask)
	if t.workspaceID != workspaceID {
		return ""
	}
	return t.task
}

// postingTask is the task a posting of amount on account in workspaceID keeps: the paying agent's side only, so the
// agent paid, and its workspace when it is another, never reads the payer's words as its own.
func postingTask(ctx context.Context, workspaceID, account string, amount int64) string {
	if amount >= 0 || !strings.HasPrefix(account, "agent:") || !agentRequestFrom(ctx).Payment {
		return ""
	}
	return taskIn(ctx, workspaceID)
}
