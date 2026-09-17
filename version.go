package jev

import (
	"fmt"
	"runtime"
	"strings"
)

// Version is the SDK version sent in the User-Agent and X-TypeSafe-SDK headers.
const Version = "0.1.0"

const userAgent = "jev-sdk-go/" + Version

// runtimeToken describes this process for the X-TypeSafe-Runtime header, in
// the same shape the official SDKs use.
var runtimeToken = fmt.Sprintf("go/%s (%s; %s)",
	strings.TrimPrefix(runtime.Version(), "go"), runtime.GOOS, runtime.GOARCH)
