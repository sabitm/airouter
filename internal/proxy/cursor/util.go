package cursor

import (
	"runtime"
	"time"
)

// runtimeVersion mimics Node's process.version in the metadata field. Go has no
// equivalent; emit the Go version prefixed so the shape is recognizable.
func runtimeVersion() string {
	return runtime.Version()
}

func timeNowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
