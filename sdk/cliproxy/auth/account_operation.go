package auth

import "context"

// WithAccountOperation serializes a credential-specific control-plane operation
// with register, update, refresh persistence, and removal. The callback receives
// a fresh snapshot and must not mutate the manager or call credential refresh.
func (m *Manager) WithAccountOperation(ctx context.Context, id string, operation func(*Auth) error) error {
	release, err := m.lockAuthMutationContext(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	current, ok := m.GetByID(id)
	if !ok {
		return &Error{Code: "auth_not_found", Message: "credential no longer registered"}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return operation(current)
}
