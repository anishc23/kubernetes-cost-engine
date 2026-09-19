package main

import "log/slog"

// logAdapter is an alias so that the CLI's construction code reads the same as
// the server's while keeping the flag-driven logger local to main.
type logAdapter = slog.Logger
