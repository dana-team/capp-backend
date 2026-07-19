package utils

// ErrContextMissing is a helper for producing a consistent internal error when
// a required context value is absent (indicates middleware misconfiguration).
func ErrContextMissing(key string) error {
	return &contextError{key: key}
}

type contextError struct{ key string }

func (e *contextError) Error() string {
	return "required context value " + e.key + " not set — check middleware order"
}
