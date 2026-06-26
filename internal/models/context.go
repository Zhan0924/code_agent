// Package models — request-scoped context helpers.
//
// 这里定义跨子包共享的 context key —— orchestrator 在 ReAct 入口写入，
// tools / memory / observability 等下游需要读取（如：拿到真实的
// userID/projectID 而不是硬编码 "default_user"）。
//
// 之所以放在 models 而不是每个调用方包内自定义同名 key：context.Value
// 比较 key 是「类型 + 值」相等性。如果 orchestrator 用自己的 contextKey
// 类型写入、tools 用自己的另一个 contextKey 类型读取，即便字符串相同
// 也读不到。把 key 收敛到 models（零依赖、所有人都已 import）是最干净
// 的跨包契约位置。
package models

import "context"

// contextKey 是私有类型，防止外部用 string literal 误覆盖。
type contextKey string

const (
	ctxKeySessionID contextKey = "session_id"
	ctxKeyUserID    contextKey = "user_id"
	ctxKeyProjectID contextKey = "project_id"
)

// WithSessionContext 把 (sessionID, userID, projectID) 三元组挂到 ctx 上。
// 调用方通常在 ReAct 入口、handler 入口处一次性挂好；后续工具/记忆/审计
// 等组件通过 SessionContextFromContext 取出，不再自己 lookup sessionMgr。
//
// 任何一个字段为空字符串都会被透传写入 —— 由调用方决定是否做归一化。
// 我们刻意不在这里做 "anonymous" / "default" 兜底，避免和 session 包的
// 归一化策略产生分歧（参见 session.AnonymousUserID 的注释）。
func WithSessionContext(ctx context.Context, sessionID, userID, projectID string) context.Context {
	if sessionID != "" {
		ctx = context.WithValue(ctx, ctxKeySessionID, sessionID)
	}
	if userID != "" {
		ctx = context.WithValue(ctx, ctxKeyUserID, userID)
	}
	if projectID != "" {
		ctx = context.WithValue(ctx, ctxKeyProjectID, projectID)
	}
	return ctx
}

// SessionIDFromContext 取出 sessionID；不存在返回 ""。
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKeySessionID).(string)
	return v
}

// UserIDFromContext 取出 userID；不存在返回 ""。
func UserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKeyUserID).(string)
	return v
}

// ProjectIDFromContext 取出 projectID；不存在返回 ""。
func ProjectIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKeyProjectID).(string)
	return v
}
