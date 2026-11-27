package web

import "forgetmenote/internal/workspace"

// JSON uploads need space for base64 and their envelope. The transport and
// outer HTTP reader share this finite budget; decoded files retain their limits.
// An archived source may also carry its uploader's text, whose JSON can escape
// each byte sixfold, as the browser's source preview allows.
func encodedUploadRequestLimit() int64 {
	upload := (max(int64(workspace.MaxFileBytes()), workspace.MaxBundleBytes()) + 2) / 3 * 4
	source := (int64(workspace.MaxSourceBytes())+2)/3*4 + int64(workspace.MaxSourceText())*6
	return max(upload, source) + (1 << 20)
}
