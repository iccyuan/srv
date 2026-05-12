package main

import "srv/internal/ui"

// `srv ui` dashboard moved to srv/internal/ui. One alias keeps the
// commands.go handler dispatch unchanged.

var cmdUI = ui.Cmd
