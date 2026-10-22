// Package apierr classifies evalsi API errors for the operator's controllers
// and admission webhook.
package apierr

import (
	"errors"
	"regexp"
	"strings"

	"connectrpc.com/connect"
)

// missingProject is the whole message servers from 0.3.1 send for a project
// that does not exist. A forbidden existing project gets "<action> on
// <resource> in project %q is not allowed (or it does not exist)" instead,
// which this must not match.
var missingProject = regexp.MustCompile(`^project "[^"]*" is not allowed \(or it does not exist\)$`)

// UnknownProject reports whether err is the API saying the request's project
// does not exist: PermissionDenied with missingProject's message from 0.3.1,
// InvalidArgument "unknown project %q" from older servers.
func UnknownProject(err error) bool {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return false
	}
	switch ce.Code() {
	case connect.CodePermissionDenied:
		return missingProject.MatchString(ce.Message())
	case connect.CodeInvalidArgument:
		return strings.Contains(ce.Message(), "unknown project")
	}
	return false
}
