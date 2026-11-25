package web

import "forgetmenote/internal/workspace"

// JSON uploads need space for base64 and their envelope. The transport and
// outer HTTP reader share this finite budget; decoded files retain their limits.
func encodedUploadRequestLimit() int64 {
	bytes := max(int64(workspace.MaxFileBytes()), workspace.MaxBundleBytes(), int64(workspace.MaxSourceBytes()))
	return (bytes+2)/3*4 + (1 << 20)
}
