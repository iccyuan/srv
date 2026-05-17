package main

import (
	"srv/internal/config"
	"srv/internal/hints"
	"srv/internal/runwrap"
	"strings"
)

// cmdRunWithHints wraps cmdRun so post-failure hints fire when the
// remote command can't be found (exit 127). Lives in main because it
// depends on cmdRun and globalOpts -- the hint engine itself moved
// into internal/hints.
func cmdRunWithHints(args []string, cfg *config.Config, opts globalOpts) error {
	wo := runwrap.Opts{
		RestartOnFail: opts.restartOnFail,
		RestartDelay:  opts.restartDelay,
		CPULimit:      opts.cpuLimit,
		MemLimit:      opts.memLimit,
	}
	err := cmdRunWithOpts(args, cfg, opts.profile, opts.tty, wo)
	rc := exitCodeOf(err)
	if rc == 127 && len(args) > 0 {
		hints.EmitTypoPostFailure(cfg, opts.noHints, strings.Join(args, " "), rc)
	}
	return err
}
