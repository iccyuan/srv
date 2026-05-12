package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type JobRecord struct {
	ID      string `json:"id"`
	Profile string `json:"profile"`
	Cmd     string `json:"cmd"`
	Cwd     string `json:"cwd"`
	Pid     int    `json:"pid"`
	Log     string `json:"log"`
	Started string `json:"started"`
}

type jobsFile struct {
	Version int          `json:"_version,omitempty"`
	Jobs    []*JobRecord `json:"jobs"`
}

func loadJobsFile() *jobsFile {
	data, err := os.ReadFile(JobsFile())
	j := &jobsFile{}
	if err != nil {
		return j
	}
	if err := json.Unmarshal(data, j); err != nil {
		// Helper used in MCP-reachable paths -- can't os.Exit.
		// Treat a corrupt jobs file as empty; the user can `srv jobs`
		// see the empty list and recover.
		fmt.Fprintf(os.Stderr, "warning: %s is not valid JSON: %v\n", JobsFile(), err)
		return j
	}
	if j.Jobs == nil {
		j.Jobs = []*JobRecord{}
	}
	warnIfNewerSchema(JobsFile(), j.Version)
	return j
}

func saveJobsFile(j *jobsFile) error {
	j.Version = SchemaVersion
	return writeJSONFile(JobsFile(), j)
}

func findJob(j *jobsFile, idOrPrefix string) *JobRecord {
	for _, job := range j.Jobs {
		if job.ID == idOrPrefix {
			return job
		}
	}
	matches := []*JobRecord{}
	for _, job := range j.Jobs {
		if strings.HasPrefix(job.ID, idOrPrefix) {
			matches = append(matches, job)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	if len(matches) > 1 {
		// Helper -- callers (cmdLogs, cmdKill, MCP tail_log/kill_job)
		// each format the error themselves. Surface ambiguity as
		// "no match" so they treat it as not-found and the user sees
		// our hint.
		fmt.Fprintf(os.Stderr, "ambiguous job id %q matches %d jobs.\n", idOrPrefix, len(matches))
		return nil
	}
	return nil
}

// spawnDetached runs `userCmd` on the remote with nohup, returns the new
// JobRecord (already persisted to jobs.json).
func spawnDetached(profileName string, profile *Profile, userCmd string) (*JobRecord, error) {
	cwd := GetCwd(profileName, profile)

	c, err := Dial(profile)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	jobID := genJobID()
	pid, err := c.RunDetached(applyRemoteEnv(profile, userCmd), cwd, jobID)
	if err != nil {
		return nil, err
	}
	rec := &JobRecord{
		ID:      jobID,
		Profile: profileName,
		Cmd:     userCmd,
		Cwd:     cwd,
		Pid:     pid,
		Log:     fmt.Sprintf("~/.srv-jobs/%s.log", jobID),
		Started: nowISO(),
	}
	jobs := loadJobsFile()
	jobs.Jobs = append(jobs.Jobs, rec)
	if err := saveJobsFile(jobs); err != nil {
		return rec, err
	}
	return rec, nil
}

func cmdDetach(args []string, cfg *Config, profileOverride string) error {
	if len(args) == 0 {
		return exitErr(1, "error: srv -d needs a command.")
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	userCmd := strings.Join(args, " ")
	rec, err := spawnDetached(name, profile, userCmd)
	if err != nil {
		return exitErr(1, "error: %v", err)
	}
	fmt.Printf("job   %s  pid=%d  profile=%s\n", rec.ID, rec.Pid, rec.Profile)
	fmt.Printf("log   %s\n", rec.Log)
	fmt.Printf("tail  srv logs %s -f\n", rec.ID)
	fmt.Printf("kill  srv kill %s\n", rec.ID)
	return nil
}

func cmdJobs(cfg *Config, profileOverride string) error {
	jobs := loadJobsFile().Jobs
	if profileOverride != "" {
		filtered := jobs[:0]
		for _, j := range jobs {
			if j.Profile == profileOverride {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}
	if len(jobs) == 0 {
		fmt.Println("(no jobs)")
		return nil
	}
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].ID < jobs[k].ID })
	for _, j := range jobs {
		cmd := j.Cmd
		if len(cmd) > 60 {
			cmd = cmd[:57] + "..."
		}
		fmt.Printf("%s  pid=%-7d profile=%-10s started=%s  cmd=%s\n",
			j.ID, j.Pid, j.Profile, j.Started, cmd)
	}
	return nil
}

func cmdLogs(args []string, cfg *Config, profileOverride string) error {
	if len(args) == 0 || args[0] == "-f" || args[0] == "--follow" {
		return exitErr(1, `usage: srv logs <id> [-f]                    output of a detached job (~/.srv-jobs/<id>.log)
see also:
  srv tail [-n N] [--grep RE] <path>            any remote file (auto-reconnect)
  srv journal -u UNIT [-f]                      systemd journal for a service`)
	}
	jid := args[0]
	follow := false
	for _, a := range args[1:] {
		if a == "-f" || a == "--follow" {
			follow = true
		}
	}
	jobs := loadJobsFile()
	j := findJob(jobs, jid)
	if j == nil {
		return exitErr(1, "error: no such job %q", jid)
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return exitErr(1, "error: profile %q (from job) not found.", j.Profile)
	}
	cmd := "cat " + j.Log
	if follow {
		cmd = "tail -f " + j.Log
	}
	return exitCode(runRemoteStream(prof, "", cmd, follow))
}

func cmdKill(args []string, cfg *Config, profileOverride string) error {
	if len(args) == 0 {
		return exitErr(1, "usage: srv kill <id>")
	}
	jid := args[0]
	sig := "TERM"
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--signal=") {
			sig = strings.TrimPrefix(a, "--signal=")
		} else if a == "-9" {
			sig = "KILL"
		}
	}
	jobs := loadJobsFile()
	j := findJob(jobs, jid)
	if j == nil {
		return exitErr(1, "error: no such job %q", jid)
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return exitErr(1, "error: profile %q (from job) not found.", j.Profile)
	}
	cmd := fmt.Sprintf("kill -%s %d 2>/dev/null && echo killed || echo 'no such pid (already exited?)'", sig, j.Pid)
	rc := runRemoteStream(prof, "", cmd, false)
	// Drop the job record.
	out := jobs.Jobs[:0]
	for _, x := range jobs.Jobs {
		if x.ID != j.ID {
			out = append(out, x)
		}
	}
	jobs.Jobs = out
	_ = saveJobsFile(jobs)
	return exitCode(rc)
}
