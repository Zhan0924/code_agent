package agentloop

import "fmt"

const (
	maxErrorHistory    = 10
	blacklistThreshold = 3 // consecutive same-tool failures to trigger blacklist
)

// AdaptiveFeedback generates context-aware feedback based on error history.
type AdaptiveFeedback struct {
	history   []ToolError
	blacklist map[string]int // tool name → consecutive failure count
}

// Record adds a tool error to the sliding window and updates blacklist state.
func (af *AdaptiveFeedback) Record(te *ToolError) {
	af.history = append(af.history, *te)
	if len(af.history) > maxErrorHistory {
		af.history = af.history[len(af.history)-maxErrorHistory:]
	}

	if af.blacklist == nil {
		af.blacklist = make(map[string]int)
	}
	af.blacklist[te.ToolName]++
}

// RecordSuccess resets the failure counter for a tool.
func (af *AdaptiveFeedback) RecordSuccess(toolName string) {
	if af.blacklist != nil {
		delete(af.blacklist, toolName)
	}
}

// IsBlacklisted returns true if a tool has exceeded the failure threshold.
func (af *AdaptiveFeedback) IsBlacklisted(toolName string) bool {
	if af.blacklist == nil {
		return false
	}
	return af.blacklist[toolName] >= blacklistThreshold
}

// SuggestAlternative returns a strategy hint when a tool is blacklisted.
func (af *AdaptiveFeedback) SuggestAlternative(toolName string) string {
	switch toolName {
	case "read_file":
		return "read_file 多次失败，改用 list_dir 或 grep 定位文件"
	case "execute_code":
		return "execute_code 不可用，改用 run_workspace_cmd 或描述预期结果"
	case "edit_file", "write_file", "patch_file":
		return fmt.Sprintf("%s 多次失败，检查文件路径是否正确或用 list_dir 确认", toolName)
	case "grep":
		return "grep 多次失败，改用 rag_search 或 read_file 逐步排查"
	default:
		return fmt.Sprintf("⚠️ %s 已连续失败 %d 次，强烈建议换用其他工具完成任务", toolName, af.blacklist[toolName])
	}
}

// BuildFeedback generates an adaptive hint message for the LLM.
func (af *AdaptiveFeedback) BuildFeedback(current *ToolError) string {
	if af.IsBlacklisted(current.ToolName) {
		return af.SuggestAlternative(current.ToolName)
	}

	repeatCount := af.countRecent(current.ToolName, current.Category)
	if repeatCount <= 1 {
		return af.firstTimeFeedback(current)
	}
	return af.repeatFeedback(current, repeatCount)
}

func (af *AdaptiveFeedback) countRecent(toolName string, cat ToolErrorCategory) int {
	count := 0
	for i := range af.history {
		if af.history[i].ToolName == toolName && af.history[i].Category == cat {
			count++
		}
	}
	return count
}

func (af *AdaptiveFeedback) firstTimeFeedback(te *ToolError) string {
	switch te.Category {
	case ErrCatInvalidArgs:
		return fmt.Sprintf("参数有误：%s。检查参数格式后重试", summarize(te.Message))
	case ErrCatNotFound:
		return fmt.Sprintf("资源不存在：%s。先用 list/search 确认正确路径", summarize(te.Message))
	case ErrCatPermission:
		return "权限不足，不要重试此工具。寻找替代方案"
	case ErrCatTimeout:
		return "超时，可重试一次"
	case ErrCatExecFailed:
		return fmt.Sprintf("命令失败：%s。分析错误输出，修改后重试", summarize(te.Message))
	default:
		return "内部错误，换用其他方法"
	}
}

func (af *AdaptiveFeedback) repeatFeedback(te *ToolError, count int) string {
	switch te.Category {
	case ErrCatInvalidArgs:
		return fmt.Sprintf("同一参数错误出现 %d 次。换用不同参数或换工具", count)
	case ErrCatNotFound:
		return fmt.Sprintf("多次找不到资源（%d 次）。确认工作目录和文件名", count)
	case ErrCatPermission:
		return "权限不足，不要重试此工具。寻找替代方案"
	case ErrCatTimeout:
		return "多次超时，跳过此操作"
	case ErrCatExecFailed:
		return fmt.Sprintf("命令持续失败（%d 次）。换一种方法解决问题", count)
	default:
		return "内部错误持续出现，换用其他方法"
	}
}

func summarize(msg string) string {
	runes := []rune(msg)
	if len(runes) > 100 {
		return string(runes[:100]) + "..."
	}
	return msg
}
