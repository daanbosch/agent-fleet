package fastvm

import (
	"errors"
	"fmt"
)

type invalidRequestError struct {
	msg string
}

func (e invalidRequestError) Error() string {
	return e.msg
}

func newInvalidRequestError(format string, args ...any) error {
	return invalidRequestError{msg: fmt.Sprintf(format, args...)}
}

func IsInvalidRequest(err error) bool {
	var target invalidRequestError
	return errors.As(err, &target)
}
