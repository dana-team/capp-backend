package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

const tokenReviewTimeout = 10 * time.Second

// tokenReviewError keeps only safe diagnostics alongside the public error.
type tokenReviewError struct {
	error
	diagnostic string
}

func (e *tokenReviewError) Unwrap() error { return e.error }

// TokenReviewDiagnostic returns safe request-log details, or empty for unrelated errors.
func TokenReviewDiagnostic(err error) string {
	var failure *tokenReviewError
	if errors.As(err, &failure) {
		return failure.diagnostic
	}
	return ""
}

// reviewToken sends the caller token only in the review body. The reviewer owns
// the transport credentials. Never return raw errors, which can contain tokens.
func reviewToken(ctx context.Context, reviewer rest.Interface, token string) (*authv1.TokenReview, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenReviewTimeout)
	defer cancel()

	// Require the response envelope and status explicitly. The typed decoder can
	// turn unrelated objects or an absent status into an empty token rejection.
	body, err := reviewer.Post().Resource("tokenreviews").SetHeader("Accept", runtime.ContentTypeJSON).Body(&authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}).Do(ctx).Raw()
	if err != nil {
		return nil, newTokenReviewError(err, ErrIdentityUnavailable)
	}
	var result struct {
		metav1.TypeMeta
		Status *authv1.TokenReviewStatus `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Kind != "TokenReview" ||
		result.APIVersion != authv1.SchemeGroupVersion.String() || result.Status == nil {
		return nil, ErrIdentityUnavailable
	}
	return &authv1.TokenReview{TypeMeta: result.TypeMeta, Status: *result.Status}, nil
}

// validateTokenReview distinguishes rejected callers from unavailable verification.
// Names are preserved exactly; an authenticated identity may have no groups.
func validateTokenReview(result *authv1.TokenReview) error {
	if result == nil || result.Status.Error != "" {
		return ErrIdentityUnavailable
	}
	status := result.Status
	if !status.Authenticated || status.User.Username == "" || status.User.Username == "system:anonymous" ||
		slices.Contains(status.User.Groups, "system:unauthenticated") {
		return ErrUnauthenticated
	}
	return nil
}

// newTokenReviewError discards upstream text and retains only status and timeout.
func newTokenReviewError(upstream, public error) error {
	var status apierrors.APIStatus
	var code int32
	if errors.As(upstream, &status) {
		code = status.Status().Code
	}
	var networkError net.Error
	timedOut := errors.Is(upstream, context.DeadlineExceeded) ||
		apierrors.IsTimeout(upstream) || apierrors.IsServerTimeout(upstream) ||
		(errors.As(upstream, &networkError) && networkError.Timeout())
	return &tokenReviewError{
		error:      public,
		diagnostic: fmt.Sprintf("token review failed: status=%d timeout=%t", code, timedOut),
	}
}
