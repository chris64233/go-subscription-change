package gosubscriptionchange

import (
	"errors"
	"fmt"
)

// 领域错误：Service 层返回这些哨兵错误（可用 errors.Is 判断），
// HTTP 层据此映射为确定的状态码。
var (
	ErrNotFound           = errors.New("resource not found")
	ErrAlreadyExists      = errors.New("resource already exists")
	ErrConflict           = errors.New("conflict")
	ErrInvalidArgument    = errors.New("invalid argument")
	ErrSubscriptionEnded  = errors.New("subscription has been terminated")
	ErrPendingChangeExist = errors.New("a pending change already exists")
	ErrConcurrentUpdate   = errors.New("concurrent update: version mismatch")
	ErrNotPending         = errors.New("change is no longer pending")
	ErrSamePlan           = errors.New("target plan is identical to current plan")
	ErrAlreadyPaid        = errors.New("change already has a confirmed payment")
)

// conflictError 携带可读细节，仍可被 errors.Is(err, ErrConflict) 命中。
type conflictError struct{ msg string }

func (e *conflictError) Error() string { return e.msg }
func (e *conflictError) Is(target error) bool {
	return target == ErrConflict
}

// conflictf 构造一个属于 ErrConflict 族的错误。
func conflictf(format string, args ...any) error {
	return &conflictError{msg: fmt.Sprintf(format, args...)}
}

// invalidArgf 构造一个属于 ErrInvalidArgument 族的错误。
func invalidArgf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// notFoundf 构造一个属于 ErrNotFound 族的错误。
func notFoundf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, args...))
}
