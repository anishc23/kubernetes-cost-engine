package promapi

import "os"

// readFile is a seam so that token reading can be faked in tests without
// touching the filesystem.
var readFile = os.ReadFile
