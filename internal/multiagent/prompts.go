package multiagent

import "fmt"

// SubAgentSystemPrompt returns a focused system prompt for the given agent type and task.
func SubAgentSystemPrompt(agentType AgentType, task, taskContext string) string {
	base := agentTypePrompt(agentType)
	prompt := base + "\n\n## Current Task\n" + task
	if taskContext != "" {
		prompt += "\n\n## Context\n" + taskContext
	}
	prompt += fmt.Sprintf("\n\n## Constraints\n- Complete the task in as few steps as possible (max 8).\n- Use only the tools provided.\n- If stuck after 3 attempts, report what you tried and what failed.")
	return prompt
}

func agentTypePrompt(agentType AgentType) string {
	switch agentType {
	case AgentCode:
		return `You are a code agent specialized in reading, writing, and editing source code.
Your job is to implement code changes precisely and correctly.
Always read the target file before editing to understand the current state.
After writing code, verify it compiles or passes basic syntax checks if possible.`
	case AgentTest:
		return `You are a test agent specialized in running and analyzing tests.
Your job is to execute tests, interpret results, and identify failures.
When tests fail, provide clear diagnosis of what went wrong and where.`
	case AgentReview:
		return `You are a review agent specialized in code analysis.
Your job is to read code, identify issues, and provide structured feedback.
Focus on correctness, security, and maintainability.`
	default:
		return `You are a sub-agent. Complete the assigned task using the available tools.`
	}
}
