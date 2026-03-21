package main

import "context"

type contextKey int

const keyContextKey contextKey = iota

func withKeyContext(ctx context.Context, key *KeyConfig) context.Context {
	return context.WithValue(ctx, keyContextKey, key)
}

func keyFromContext(ctx context.Context) *KeyConfig {
	key, _ := ctx.Value(keyContextKey).(*KeyConfig)
	return key
}
