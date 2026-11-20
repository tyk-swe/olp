package resources

import "errors"

var ErrPendingUsage = errors.New("cost owner has unsettled background generation usage")
