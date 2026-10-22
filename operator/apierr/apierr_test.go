package apierr

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
)

func TestUnknownProject(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		// What 0.3.1 servers send for a missing project, also when wrapped.
		{connect.NewError(connect.CodePermissionDenied, errors.New(`project "e2e" is not allowed (or it does not exist)`)), true},
		{fmt.Errorf("check run: %w", connect.NewError(connect.CodePermissionDenied, errors.New(`project "team-a" is not allowed (or it does not exist)`))), true},
		// What older servers send.
		{connect.NewError(connect.CodeInvalidArgument, errors.New(`unknown project "e2e"`)), true},
		// A forbidden action in an existing project is a real denial.
		{connect.NewError(connect.CodePermissionDenied, errors.New(`runs.create on run/parity in project "e2e" is not allowed (or it does not exist)`)), false},
		{connect.NewError(connect.CodeInvalidArgument, errors.New(`spec: evaluators is required`)), false},
		{connect.NewError(connect.CodeUnavailable, errors.New(`project "e2e" is not allowed (or it does not exist)`)), false},
		{errors.New(`project "e2e" is not allowed (or it does not exist)`), false},
	} {
		if got := UnknownProject(c.err); got != c.want {
			t.Errorf("UnknownProject(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
