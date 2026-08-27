package plan

import "context"

type goalRoundContextKey struct{}

// WithGoalRound marks the context used by an automatic same-session goal
// continuation. Goal tools use this marker to apply the stricter DSH authority
// and blocked-round rules without treating every non-CLI call as autonomous.
func WithGoalRound(ctx context.Context) context.Context {
	return context.WithValue(ctx, goalRoundContextKey{}, true)
}

func IsGoalRound(ctx context.Context) bool {
	marked, _ := ctx.Value(goalRoundContextKey{}).(bool)
	return marked
}
