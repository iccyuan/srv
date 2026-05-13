package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"srv/internal/config"
	"srv/internal/progress"
	"srv/internal/remote"
	"srv/internal/session"
	"srv/internal/syncx"
	"srv/internal/transfer"
	"strings"
	"time"
)

func handleDiff(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	local, _ := args["local"].(string)
	if local == "" {
		return textErr("local is required")
	}
	remotePath, _ := args["remote"].(string)
	text, rc, err := deps.Diff(cfg, profileOverride, local, remotePath)
	if err != nil {
		return textErr(err.Error())
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           rc != 0,
		StructuredContent: map[string]any{"exit_code": rc, "local": local, "remote": remotePath},
	}
}

func handlePush(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	local, _ := args["local"].(string)
	if local == "" {
		return textErr("local is required")
	}
	if _, err := os.Stat(local); err != nil {
		return textErr(fmt.Sprintf("local path missing: %q", local))
	}
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := config.GetCwd(profName, prof)
	rpath, _ := args["remote"].(string)
	if rpath == "" {
		rpath = filepath.Base(local)
	}
	abs := remote.ResolvePath(rpath, cwd)
	st, _ := os.Stat(local)
	recursive := false
	if rb, ok := args["recursive"].(bool); ok {
		recursive = rb
	}
	if st != nil && st.IsDir() {
		recursive = true
	}
	start := time.Now()
	rc, finalRemote, perr := transfer.PushPath(prof, local, abs, recursive)
	duration := time.Since(start)
	var bytes int64
	if rc == 0 {
		bytes = progress.SumLocalSize(local)
	}
	var text string
	if rc == 0 {
		text = fmt.Sprintf("uploaded %s -> %s [exit 0]%s", local, finalRemote, progress.FmtRate(bytes, duration))
	} else {
		text = fmt.Sprintf("upload FAILED %s -> %s [exit %d]", local, finalRemote, rc)
		if perr != nil {
			text += ": " + perr.Error()
		}
	}
	structured := map[string]any{
		"exit_code":        rc,
		"remote":           finalRemote,
		"local":            local,
		"duration_seconds": duration.Seconds(),
	}
	if rc == 0 {
		structured["bytes_transferred"] = bytes
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           rc != 0,
		StructuredContent: structured,
	}
}

func handlePull(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	rpath, _ := args["remote"].(string)
	if rpath == "" {
		return textErr("remote is required")
	}
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := config.GetCwd(profName, prof)
	local, _ := args["local"].(string)
	if local == "" {
		local = "."
	}
	abs := remote.ResolvePath(rpath, cwd)
	recursive := false
	if rb, ok := args["recursive"].(bool); ok {
		recursive = rb
	}
	start := time.Now()
	rc, finalLocal, perr := transfer.PullPath(prof, abs, local, recursive)
	duration := time.Since(start)
	var bytes int64
	if rc == 0 {
		bytes = progress.SumLocalSize(finalLocal)
	}
	var text string
	if rc == 0 {
		text = fmt.Sprintf("downloaded %s -> %s [exit 0]%s", abs, finalLocal, progress.FmtRate(bytes, duration))
	} else {
		text = fmt.Sprintf("download FAILED %s -> %s [exit %d]", abs, finalLocal, rc)
		if perr != nil {
			text += ": " + perr.Error()
		}
	}
	structured := map[string]any{
		"exit_code":        rc,
		"remote":           abs,
		"local":            finalLocal,
		"duration_seconds": duration.Seconds(),
	}
	if rc == 0 {
		structured["bytes_transferred"] = bytes
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           rc != 0,
		StructuredContent: structured,
	}
}

func handleSync(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	o := &syncx.Options{GitScope: "all"}
	if v, ok := args["remote_root"].(string); ok {
		o.RemoteRoot = v
	}
	if v, ok := args["mode"].(string); ok {
		o.Mode = v
	}
	if v, ok := args["git_scope"].(string); ok {
		o.GitScope = v
	}
	if v, ok := args["since"].(string); ok {
		o.Since = v
	}
	if v, ok := args["root"].(string); ok {
		o.Root = v
	}
	if v, ok := args["dry_run"].(bool); ok {
		o.DryRun = v
	}
	if v, ok := args["delete"].(bool); ok {
		o.Delete = v
	}
	if v, ok := args["yes"].(bool); ok {
		o.Yes = v
	}
	if o.Delete && !o.DryRun && session.GuardOn() {
		confirm, _ := args["confirm"].(bool)
		if !confirm {
			return guardBlocked("sync",
				"delete=true would remove remote files")
		}
	}
	if v, ok := args["delete_limit"].(float64); ok {
		o.DeleteLimit = int(v)
	}
	if v, ok := args["include"].([]any); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				o.Include = append(o.Include, s)
			}
		}
	}
	if v, ok := args["exclude"].([]any); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				o.Exclude = append(o.Exclude, s)
			}
		}
	}
	if v, ok := args["files"].([]any); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				o.Files = append(o.Files, s)
			}
		}
	}
	localRoot := o.Root
	if localRoot == "" {
		localRoot = syncx.FindGitRoot(syncx.MustCwd())
		if localRoot == "" {
			localRoot = syncx.MustCwd()
		}
	}
	if o.Mode == "" {
		if syncx.FindGitRoot(localRoot) != "" {
			o.Mode = "git"
		} else if len(o.Include) > 0 {
			o.Mode = "glob"
		} else if o.Since != "" {
			o.Mode = "mtime"
		} else if len(o.Files) > 0 {
			o.Mode = "list"
		} else {
			return textErr("no mode resolved (not a git repo and no include/since/files)")
		}
	}
	cwd := config.GetCwd(profName, prof)
	remoteRoot := cwd
	if o.RemoteRoot != "" {
		remoteRoot = remote.ResolvePath(o.RemoteRoot, cwd)
	} else if prof.SyncRoot != "" {
		remoteRoot = remote.ResolvePath(prof.SyncRoot, cwd)
	}
	allExcludes := append([]string{}, o.Exclude...)
	allExcludes = append(allExcludes, prof.SyncExclude...)
	allExcludes = append(allExcludes, syncx.DefaultExcludes...)
	files, err := syncx.CollectFiles(o, localRoot, allExcludes)
	if err != nil {
		return textErr(err.Error())
	}
	deletes, err := syncx.CollectDeletes(o, localRoot, allExcludes)
	if err != nil {
		return textErr(err.Error())
	}
	limit := o.DeleteLimit
	if limit == 0 {
		limit = 20
	}
	if len(deletes) > limit && !o.DryRun && !o.Yes {
		return textErr(fmt.Sprintf("sync delete would remove %d files (limit %d). Re-run dry_run=true, yes=true, or set delete_limit.", len(deletes), limit))
	}
	if len(files) == 0 && len(deletes) == 0 {
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: "(nothing to sync)"}},
			StructuredContent: map[string]any{"files": []string{}, "deletes": deletes, "remote_root": remoteRoot, "exit_code": 0},
		}
	}
	if o.DryRun {
		text := fmt.Sprintf("would sync %d files to %s\n", len(files), remoteRoot)
		lim := len(files)
		if lim > 200 {
			lim = 200
		}
		text += strings.Join(files[:lim], "\n")
		if len(files) > 200 {
			text += fmt.Sprintf("\n... (%d more)", len(files)-200)
		}
		if len(deletes) > 0 {
			text += "\nwould Delete:\n" + strings.Join(deletes, "\n")
		}
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
			StructuredContent: map[string]any{
				"files_count":   len(files),
				"deletes_count": len(deletes),
				"remote_root":   remoteRoot,
				"dry_run":       true,
			},
		}
	}
	rc := 0
	var terr error
	start := time.Now()
	if len(files) > 0 {
		rc, terr = syncx.TarUploadStream(prof, localRoot, files, remoteRoot)
	}
	if rc == 0 && len(deletes) > 0 {
		rc, terr = syncx.DeleteRemoteFiles(prof, remoteRoot, deletes)
	}
	duration := time.Since(start)
	var bytes int64
	if rc == 0 {
		for _, f := range files {
			if st, err := os.Stat(filepath.Join(localRoot, f)); err == nil {
				bytes += st.Size()
			}
		}
	}
	var text string
	if rc == 0 {
		text = fmt.Sprintf("synced %d files to %s [exit 0]%s", len(files), remoteRoot, progress.FmtRate(bytes, duration))
	} else {
		text = fmt.Sprintf("sync FAILED to %s [exit %d]; %d files were NOT transferred -- verify with `run \"ls -la %s\"` before assuming",
			remoteRoot, rc, len(files), remoteRoot)
	}
	if terr != nil {
		text += ": " + terr.Error()
	}
	structured := map[string]any{
		"files_count":      len(files),
		"deletes_count":    len(deletes),
		"remote_root":      remoteRoot,
		"exit_code":        rc,
		"duration_seconds": duration.Seconds(),
	}
	if rc == 0 {
		structured["bytes_transferred"] = bytes
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           rc != 0,
		StructuredContent: structured,
	}
}

func handleSyncDeleteDryRun(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	root, _ := args["root"].(string)
	if root == "" {
		root = syncx.FindGitRoot(syncx.MustCwd())
	}
	if root == "" {
		return textErr("not in a git repo")
	}
	root, _ = filepath.Abs(root)
	o := &syncx.Options{Mode: "git", GitScope: "all", Delete: true, DryRun: true}
	allExcludes := append([]string{}, prof.SyncExclude...)
	allExcludes = append(allExcludes, syncx.DefaultExcludes...)
	deletes, err := syncx.CollectDeletes(o, root, allExcludes)
	if err != nil {
		return textErr(err.Error())
	}
	cwd := config.GetCwd(profName, prof)
	remoteRoot := cwd
	if v, _ := args["remote_root"].(string); v != "" {
		remoteRoot = remote.ResolvePath(v, cwd)
	} else if prof.SyncRoot != "" {
		remoteRoot = remote.ResolvePath(prof.SyncRoot, cwd)
	}
	text := fmt.Sprintf("would delete %d files from %s\n%s", len(deletes), remoteRoot, strings.Join(deletes, "\n"))
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		StructuredContent: map[string]any{
			"deletes_count": len(deletes),
			"remote_root":   remoteRoot,
			"dry_run":       true,
		},
	}
}

func handleListDir(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	path, _ := args["path"].(string)
	dirsOnly, _ := args["dirs_only"].(bool)
	limit := 500
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	entries, err := deps.ListEntries(path, cfg, profileOverride)
	if err != nil {
		return textErr(err.Error())
	}
	if dirsOnly {
		kept := entries[:0]
		for _, e := range entries {
			if strings.HasSuffix(e, "/") {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	truncated := 0
	if len(entries) > limit {
		truncated = len(entries) - limit
		entries = entries[:limit]
	}
	text := strings.Join(entries, "\n")
	if text != "" {
		text += "\n"
	}
	structured := map[string]any{
		"entries": entries,
		"count":   len(entries),
	}
	if truncated > 0 {
		structured["truncated_count"] = truncated
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		StructuredContent: structured,
	}
}
