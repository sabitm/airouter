package kirocatalog

import "context"

func contextCanceled() error { return context.Canceled }
func contextDeadline() error { return context.DeadlineExceeded }
